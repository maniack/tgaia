package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/queue"
	"github.com/maniack/tgaia/internal/storage"
)

// Terminal tools of the background runs. Compaction and image description are
// ordinary runs of the bot with its own prompt; only the event and the
// terminal tool differ.
const (
	toolSummary  = "memory_save_summary"
	toolDescribe = "image_describe"
)

func summaryTool(limit int) llm.Tool {
	params := fmt.Sprintf(`{"type":"object","additionalProperties":false,"properties":{
"summary":{"type":"string","maxLength":%d,"description":"Summary of the events and facts from the current memory together with the new messages. Replaces the current memory."},
"corrections":{"type":"array","maxItems":3,"description":"Changes to your own behaviour in this chat that the conversation calls for, e.g. when people corrected or complained about your answers. Empty when nothing needs to change. Existing ones are listed under Behaviour corrections.","items":{"type":"object","additionalProperties":false,"properties":{"text":{"type":"string","maxLength":%d,"description":"The behaviour to adopt, as a short rule."},"reason":{"type":"string","maxLength":%d,"description":"What in the conversation shows it is needed."}},"required":["text","reason"]}}},
"required":["summary","corrections"]}`, limit, storage.CorrectionTextLimit, storage.CorrectionTextLimit)
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: toolSummary, Description: "Save the updated long-term memory of this chat and end the turn.", Parameters: json.RawMessage(params)}}
}

var describeTool = llm.Tool{Type: "function", Function: llm.ToolFunction{Name: toolDescribe, Description: "Save a description of the attached image, including any readable text, to the conversation history and end the turn.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"text":{"type":"string","maxLength":4000}},"required":["text"]}`)}}

func summaryOf(r turnResult) (string, []agent.Correction, error) {
	if r.Name != toolSummary {
		if text := strings.TrimSpace(r.Prose); text != "" {
			return text, nil, nil
		}
		return "", nil, errors.New("model returned no summary")
	}
	var args struct {
		Summary     string             `json:"summary"`
		Corrections []agent.Correction `json:"corrections"`
	}
	if json.Unmarshal([]byte(r.Arguments), &args) != nil || strings.TrimSpace(args.Summary) == "" {
		return "", nil, errors.New("model returned invalid compaction arguments")
	}
	return args.Summary, args.Corrections, nil
}

func describedText(r turnResult) string {
	if r.Name == toolDescribe {
		var args struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(r.Arguments), &args) == nil {
			return strings.TrimSpace(args.Text)
		}
		return ""
	}
	return strings.TrimSpace(r.Prose)
}

func (e *Engine) proposeCorrections(ctx context.Context, job queue.Job, proposals []agent.Correction) error {
	return e.DB.Agent().ProposeCorrections(ctx, job.Scope, proposals)
}

// correctionsContext lists a chat's corrections as data. The reply run sees
// only approved ones; compaction also sees pending and disabled ones, so it
// does not propose them again.
func (e *Engine) correctionsContext(ctx context.Context, scope chat.Scope, all bool) (string, error) {
	status := "approved"
	if all {
		status = ""
	}
	list, err := e.DB.Agent().Corrections(ctx, scope, status)
	if err != nil || len(list) == 0 {
		return "", err
	}
	var out strings.Builder
	out.WriteString("\n## Behaviour corrections\n")
	for _, c := range list {
		if all {
			fmt.Fprintf(&out, "- [%s] %s\n", c.Status, c.Text)
		} else {
			fmt.Fprintf(&out, "- %s\n", c.Text)
		}
	}
	return out.String(), nil
}

// DescribeImage runs the bot (its own prompt) on a single image event outside
// any job, e.g. its own avatar at startup. It returns the description text.
func DescribeImage(ctx context.Context, model Model, system string, event map[string]any, image string) (string, error) {
	data, _ := json.Marshal(event)
	messages := []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: "## Event\n" + string(data) + "\n", Images: []string{image}}}
	ctx = llm.WithThinking(llm.WithOutputBudget(ctx, 1024), false)
	if tools, ok := model.(toolModel); ok {
		message, err := tools.CompleteTools(llm.WithToolChoice(ctx, "required"), messages, []llm.Tool{describeTool})
		if err != nil {
			return "", err
		}
		for _, c := range message.ToolCalls {
			if c.Function.Name == toolDescribe {
				return describedText(turnResult{Name: toolDescribe, Arguments: c.Function.Arguments}), nil
			}
		}
		return strings.TrimSpace(message.Content), nil
	}
	text, err := model.Complete(ctx, messages)
	return strings.TrimSpace(text), err
}

// BotPrompt resolves a bot's single prompt: its database text, or the default file.
func BotPrompt(ctx context.Context, db *storage.Database, agentID string, botID int64, defaultFile string, maxBytes int) (string, error) {
	prompt, err := db.Bots().Prompt(ctx, agentID, botID)
	if err == nil && strings.TrimSpace(prompt) != "" {
		return prompt, nil
	}
	return readDefaultPrompt(defaultFile, maxBytes)
}
