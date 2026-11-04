package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go.opentelemetry.io/otel"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/maniack/tgaia/internal/config"
)

type Message struct {
	Images           []string   `json:"-"`
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type Client struct {
	inputBytes, outputTokens int
	endpoint                 string
	model                    string
	key                      config.Secret
	thinking                 bool
	http                     *http.Client
	slots                    chan struct{}
}

func New(endpoint, model string, key config.Secret) (*Client, error) {
	return NewWithLimits(endpoint, model, key, config.App{LLMThinking: true})
}
func NewWithLimits(endpoint, model string, key config.Secret, cfg config.App) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || model == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid LLM configuration")
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
		return nil, errors.New("LLM HTTP is allowed only on loopback")
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	u.Path = path + "/chat/completions"
	return &Client{inputBytes: cfg.Limit("llm_input_bytes"), outputTokens: cfg.Limit("llm_output_tokens"), endpoint: u.String(), model: model, key: key, thinking: cfg.LLMThinking, http: &http.Client{Timeout: cfg.Duration("llm_timeout"), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, slots: make(chan struct{}, cfg.Limit("llm_concurrency"))}, nil
}
func (c *Client) Complete(ctx context.Context, messages []Message) (string, error) {
	return c.complete(ctx, messages, nil)
}
func (c *Client) CompleteJSON(ctx context.Context, messages []Message, schema json.RawMessage) (string, error) {
	return c.complete(ctx, messages, schema)
}
func (c *Client) complete(ctx context.Context, messages []Message, schema json.RawMessage) (string, error) {
	result, err := c.chat(ctx, messages, schema, nil)
	return result.Content, err
}
func (c *Client) CompleteTools(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	return c.chat(ctx, messages, nil, tools)
}
func (c *Client) chat(ctx context.Context, messages []Message, schema json.RawMessage, tools []Tool) (Message, error) {
	ctx, span := otel.Tracer("tgaia").Start(ctx, "llm.chat")
	defer span.End()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
	total := 0
	for _, m := range messages {
		if len(m.Images) > 4 {
			return Message{}, errors.New("too many images")
		}
		for _, image := range m.Images {
			if len(image) > 12<<20 || !strings.HasPrefix(image, "data:image/") {
				return Message{}, errors.New("invalid image input")
			}
		}
		m.Images = nil
		encoded, _ := json.Marshal(m)
		total += len(encoded)
	}
	if total > c.inputBytes {
		return Message{}, errors.New("LLM input budget exceeded")
	}
	request := map[string]any{"model": c.model, "messages": messages, "max_tokens": c.outputTokens, "temperature": 0.2}
	if n, ok := ctx.Value(outputBudgetKey{}).(int); ok && n > 0 && n <= 65536 {
		request["max_tokens"] = n
	}
	if schema != nil {
		request["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "agent_response", "strict": true, "schema": schema}}
	}
	if len(tools) > 0 {
		request["tools"] = tools
		request["tool_choice"] = "auto"
		if choice, ok := ctx.Value(toolChoiceKey{}).(string); ok && choice != "" {
			request["tool_choice"] = choice
		}
		request["parallel_tool_calls"] = false
		if c.thinking {
			request["chat_template_kwargs"] = map[string]any{"enable_thinking": true}
		}
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > 16<<20 {
		return Message{}, errors.New("encoding model request failed or budget exceeded")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Message{}, errors.New("invalid model request")
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key.Reveal() != "" {
		req.Header.Set("Authorization", "Bearer "+c.key.Reveal())
	}
	response, err := c.http.Do(req)
	if err != nil {
		return Message{}, errors.New("LLM request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Message{}, errors.New("LLM returned unsuccessful status")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return Message{}, errors.New("invalid LLM response size")
	}
	var result struct {
		Choices []struct {
			Message struct {
				Role             string     `json:"role"`
				Content          *string    `json:"content"`
				ReasoningContent *string    `json:"reasoning_content"`
				Reasoning        *string    `json:"reasoning"`
				Thought          *string    `json:"thought"`
				ToolCalls        []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &result); err != nil || len(result.Choices) != 1 || result.Choices[0].FinishReason == "length" {
		return Message{}, errors.New("incomplete model response")
	}
	choice := result.Choices[0]
	var content, reasoning string
	if choice.Message.Content != nil {
		content = *choice.Message.Content
	}
	if choice.Message.ReasoningContent != nil {
		reasoning = *choice.Message.ReasoningContent
	} else if choice.Message.Reasoning != nil {
		reasoning = *choice.Message.Reasoning
	} else if choice.Message.Thought != nil {
		reasoning = *choice.Message.Thought
	}
	if content == "" && reasoning == "" && len(choice.Message.ToolCalls) == 0 {
		return Message{}, errors.New("incomplete model response")
	}
	message := Message{
		Role:             choice.Message.Role,
		Content:          content,
		ReasoningContent: reasoning,
		ToolCalls:        choice.Message.ToolCalls,
	}
	if message.Role != "assistant" || len(message.ToolCalls) > 8 || (len(tools) == 0 && len(message.ToolCalls) > 0) {
		return Message{}, errors.New("invalid model tool response")
	}
	seen := map[string]bool{}
	for _, call := range message.ToolCalls {
		if call.ID == "" || len(call.ID) > 256 || seen[call.ID] || call.Type != "function" || call.Function.Name == "" || len(call.Function.Arguments) > 32000 {
			return Message{}, errors.New("invalid model tool call")
		}
		seen[call.ID] = true
	}
	return message, nil
}

type outputBudgetKey struct{}
type toolChoiceKey struct{}

func WithOutputBudget(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, outputBudgetKey{}, n)
}

func WithToolChoice(ctx context.Context, choice string) context.Context {
	return context.WithValue(ctx, toolChoiceKey{}, choice)
}
