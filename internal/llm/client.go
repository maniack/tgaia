package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/queue"
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

// Failure classes reported through queue.Classify. Timeouts and truncation are
// deterministic for a given prompt, so they retire the job instead of retrying.
const (
	ClassTimeout   = "llm_timeout"
	ClassRateLimit = "llm_429"
	ClassUpstream  = "llm_http_5xx"
	ClassRejected  = "llm_http_4xx"
	ClassNetwork   = "llm_network"
	ClassTruncated = "llm_truncated"
	ClassInvalid   = "llm_invalid"
	ClassBudget    = "llm_budget"
)

var (
	errFirstToken = errors.New("LLM first token timeout")
	errIdle       = errors.New("LLM stream idle timeout")
	errCeiling    = errors.New("LLM total timeout")
)

const (
	maxStreamBytes = 4 << 20 // Total SSE payload accepted per request.
	maxOutputBytes = 1 << 20 // Assembled content, reasoning and arguments.
)

type Client struct {
	inputBytes, outputTokens  int
	endpoint                  string
	model                     string
	key                       config.Secret
	thinking                  bool
	ceiling, firstToken, idle time.Duration
	http                      *http.Client
	slots                     chan struct{}
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
	// No http.Client.Timeout: it would cover the whole streamed body. Liveness is
	// enforced per request by the first-token and idle watchdogs below, and the
	// ceiling bounds total generation time.
	return &Client{inputBytes: cfg.Limit("llm_input_bytes"), outputTokens: cfg.Limit("llm_output_tokens"), endpoint: u.String(), model: model, key: key, thinking: cfg.LLMThinking, ceiling: cfg.Duration("llm_timeout"), firstToken: cfg.Duration("llm_first_token_timeout"), idle: cfg.Duration("llm_idle_timeout"), http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, slots: make(chan struct{}, cfg.Limit("llm_concurrency"))}, nil
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

func terminal(class string, text string) error { return queue.Fail(class, true, errors.New(text)) }
func transient(class string, text string) error {
	return queue.Fail(class, false, errors.New(text))
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
			return Message{}, terminal(ClassBudget, "too many images")
		}
		for _, image := range m.Images {
			if len(image) > 12<<20 || !strings.HasPrefix(image, "data:image/") {
				return Message{}, terminal(ClassBudget, "invalid image input")
			}
		}
		m.Images = nil
		encoded, _ := json.Marshal(m)
		total += len(encoded)
	}
	if total > c.inputBytes {
		return Message{}, terminal(ClassBudget, "LLM input budget exceeded")
	}
	request := map[string]any{"model": c.model, "messages": messages, "max_tokens": c.outputTokens, "temperature": 0.2, "stream": true, "stream_options": map[string]any{"include_usage": true}}
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
		return Message{}, terminal(ClassBudget, "encoding model request failed or budget exceeded")
	}

	// Three independent timeouts. The ceiling is the only one measured over the
	// whole request; the watchdogs measure silence, so a live stream may legally
	// outlast any proxy's per-request timeout.
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if c.ceiling > 0 {
		ceiling := time.AfterFunc(c.ceiling, func() { cancel(errCeiling) })
		defer ceiling.Stop()
	}
	var watchdog *time.Timer
	if c.firstToken > 0 {
		watchdog = time.AfterFunc(c.firstToken, func() { cancel(errFirstToken) })
		defer watchdog.Stop()
	}
	alive := func() {
		if watchdog == nil {
			if c.idle > 0 {
				watchdog = time.AfterFunc(c.idle, func() { cancel(errIdle) })
			}
			return
		}
		if c.idle > 0 {
			watchdog.Reset(c.idle)
		} else {
			watchdog.Stop()
		}
	}
	classify := func(fallback string) error {
		switch cause := context.Cause(reqCtx); {
		case errors.Is(cause, errFirstToken), errors.Is(cause, errIdle), errors.Is(cause, errCeiling):
			return terminal(ClassTimeout, cause.Error())
		case ctx.Err() != nil:
			return ctx.Err()
		}
		return transient(ClassNetwork, fallback)
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Message{}, terminal(ClassInvalid, "invalid model request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	if c.key.Reveal() != "" {
		req.Header.Set("Authorization", "Bearer "+c.key.Reveal())
	}
	response, err := c.http.Do(req)
	if err != nil {
		return Message{}, classify("LLM request failed")
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusOK:
	case response.StatusCode == http.StatusTooManyRequests:
		return Message{}, transient(ClassRateLimit, "LLM rate limited")
	case response.StatusCode >= 500:
		return Message{}, transient(ClassUpstream, "LLM upstream failure")
	default:
		return Message{}, terminal(ClassRejected, "LLM rejected the request")
	}
	reader := bufio.NewReaderSize(io.LimitReader(response.Body, maxStreamBytes+1), 64<<10)
	first, err := peekNonSpace(reader)
	if err != nil {
		return Message{}, classify("LLM response unreadable")
	}
	alive()
	var result assembled
	if first == '{' {
		// A gateway that ignores stream=true answers with a plain JSON body.
		result, err = readJSON(reader)
	} else {
		result, err = readStream(reader, alive)
	}
	if err != nil {
		if reqCtx.Err() != nil || ctx.Err() != nil {
			return Message{}, classify("LLM stream interrupted")
		}
		return Message{}, err
	}
	return c.validate(result, tools)
}

type assembled struct {
	role, content, reasoning, finish string
	calls                            []ToolCall
}

func peekNonSpace(r *bufio.Reader) (byte, error) {
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			return b, r.UnreadByte()
		}
	}
}

type rawMessage struct {
	Role             string  `json:"role"`
	Content          *string `json:"content"`
	ReasoningContent *string `json:"reasoning_content"`
	Reasoning        *string `json:"reasoning"`
	Thought          *string `json:"thought"`
	ToolCalls        []struct {
		Index    *int   `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func (m rawMessage) reasoning() string {
	switch {
	case m.ReasoningContent != nil:
		return *m.ReasoningContent
	case m.Reasoning != nil:
		return *m.Reasoning
	case m.Thought != nil:
		return *m.Thought
	}
	return ""
}

func readJSON(r io.Reader) (assembled, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxOutputBytes+1))
	if err != nil || len(data) > maxOutputBytes {
		return assembled{}, transient(ClassInvalid, "invalid LLM response size")
	}
	var body struct {
		Choices []struct {
			Message      rawMessage `json:"message"`
			FinishReason string     `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &body); err != nil || len(body.Choices) != 1 {
		return assembled{}, transient(ClassInvalid, "incomplete model response")
	}
	choice := body.Choices[0]
	out := assembled{role: choice.Message.Role, reasoning: choice.Message.reasoning(), finish: choice.FinishReason}
	if choice.Message.Content != nil {
		out.content = *choice.Message.Content
	}
	for _, call := range choice.Message.ToolCalls {
		out.calls = append(out.calls, ToolCall{ID: call.ID, Type: call.Type, Function: ToolFunction{Name: call.Function.Name, Arguments: call.Function.Arguments}})
	}
	return out, nil
}

// readStream assembles OpenAI-style SSE deltas. Tool calls arrive in fragments
// keyed by index; arguments are concatenated in order of arrival.
func readStream(r *bufio.Reader, alive func()) (assembled, error) {
	var out assembled
	var content, reasoning strings.Builder
	type partial struct {
		call ToolCall
		args strings.Builder
	}
	calls := map[int]*partial{}
	size := 0
	seen := 0
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			seen += len(line)
			if seen > maxStreamBytes {
				return assembled{}, transient(ClassInvalid, "invalid LLM response size")
			}
			line = bytes.TrimRight(line, "\r\n")
			if payload, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				payload = bytes.TrimSpace(payload)
				if bytes.Equal(payload, []byte("[DONE]")) {
					break
				}
				alive()
				var chunk struct {
					Choices []struct {
						Delta        rawMessage `json:"delta"`
						FinishReason *string    `json:"finish_reason"`
					} `json:"choices"`
					Error json.RawMessage `json:"error"`
				}
				if json.Unmarshal(payload, &chunk) != nil {
					return assembled{}, transient(ClassInvalid, "invalid LLM stream chunk")
				}
				if len(chunk.Error) > 0 && !bytes.Equal(chunk.Error, []byte("null")) {
					return assembled{}, transient(ClassUpstream, "LLM reported a stream error")
				}
				for _, choice := range chunk.Choices {
					delta := choice.Delta
					if delta.Role != "" {
						out.role = delta.Role
					}
					if delta.Content != nil {
						content.WriteString(*delta.Content)
						size += len(*delta.Content)
					}
					if text := delta.reasoning(); text != "" {
						reasoning.WriteString(text)
						size += len(text)
					}
					for i, fragment := range delta.ToolCalls {
						index := i
						if fragment.Index != nil {
							index = *fragment.Index
						}
						if index < 0 || index > 63 {
							return assembled{}, transient(ClassInvalid, "invalid LLM tool call index")
						}
						p := calls[index]
						if p == nil {
							p = &partial{}
							calls[index] = p
						}
						if fragment.ID != "" {
							p.call.ID = fragment.ID
						}
						if fragment.Type != "" {
							p.call.Type = fragment.Type
						}
						if fragment.Function.Name != "" {
							p.call.Function.Name += fragment.Function.Name
						}
						p.args.WriteString(fragment.Function.Arguments)
						size += len(fragment.Function.Arguments)
					}
					if choice.FinishReason != nil && *choice.FinishReason != "" {
						out.finish = *choice.FinishReason
					}
				}
				if size > maxOutputBytes {
					return assembled{}, transient(ClassInvalid, "invalid LLM response size")
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Some gateways close the stream without a [DONE] sentinel.
				break
			}
			return assembled{}, transient(ClassNetwork, "LLM stream read failed")
		}
	}
	out.content = content.String()
	out.reasoning = reasoning.String()
	indexes := make([]int, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		p := calls[index]
		if p.call.Type == "" {
			p.call.Type = "function"
		}
		p.call.Function.Arguments = p.args.String()
		out.calls = append(out.calls, p.call)
	}
	if out.role == "" {
		out.role = "assistant"
	}
	return out, nil
}

func (c *Client) validate(result assembled, tools []Tool) (Message, error) {
	if result.finish == "length" {
		// Truncated output is deterministic for the prompt and, under json_schema,
		// never valid JSON. Retrying would repeat the same generation.
		return Message{}, terminal(ClassTruncated, "model output truncated by token limit")
	}
	if result.content == "" && result.reasoning == "" && len(result.calls) == 0 {
		return Message{}, transient(ClassInvalid, "incomplete model response")
	}
	message := Message{Role: result.role, Content: result.content, ReasoningContent: result.reasoning, ToolCalls: result.calls}
	if message.Role != "assistant" || len(message.ToolCalls) > 8 || (len(tools) == 0 && len(message.ToolCalls) > 0) {
		return Message{}, transient(ClassInvalid, "invalid model tool response")
	}
	seen := map[string]bool{}
	for _, call := range message.ToolCalls {
		if call.ID == "" || len(call.ID) > 256 || seen[call.ID] || call.Type != "function" || call.Function.Name == "" || len(call.Function.Arguments) > 32000 {
			return Message{}, transient(ClassInvalid, "invalid model tool call")
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

// OutputBudget returns the per-call max_tokens override carried by ctx, or 0.
func OutputBudget(ctx context.Context) int {
	n, _ := ctx.Value(outputBudgetKey{}).(int)
	return n
}

func WithToolChoice(ctx context.Context, choice string) context.Context {
	return context.WithValue(ctx, toolChoiceKey{}, choice)
}

// ToolChoice returns the tool_choice override carried by ctx, or "".
func ToolChoice(ctx context.Context) string {
	choice, _ := ctx.Value(toolChoiceKey{}).(string)
	return choice
}
