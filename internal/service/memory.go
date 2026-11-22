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
"summary":{"type":"string","maxLength":%d,"description":"Updated long-term memory of this chat. Copy every dated event line from ## Current memory into this summary unchanged, then append new events from ## New messages. Never drop or merge existing dated entries — historical record must be cumulative. Write in the same language the chat participants use. Record events with their date (e.g. '2026-11-10: alert fired'), not as current states ('alert is active'). Explicit memory instructions from users must be stored as directives to yourself in imperative form ('Reply only in Russian'), not as descriptions of the user ('user wants Russian')."}},
"required":["summary"]}`, limit)
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: toolSummary, Description: "Save the updated long-term memory of this chat and end the turn.", Parameters: json.RawMessage(params)}}
}

var describeTool = llm.Tool{Type: "function", Function: llm.ToolFunction{Name: toolDescribe, Description: "Save a description of the attached image, including any readable text, to the conversation history and end the turn.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"text":{"type":"string","maxLength":4000}},"required":["text"]}`)}}

const toolReviewDone = "review_done"

var reviewTool = llm.Tool{Type: "function", Function: llm.ToolFunction{Name: toolReviewDone,
	Description: "Submit the behaviour review result and end the turn.",
	Parameters: json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,"properties":{
"operations":{"type":"array","maxItems":8,"description":"Operations to apply to the behaviour correction list. Use 'add' for new rules; use 'replace', 'merge', or 'remove' to consolidate when the list is near its 10-rule limit.","items":{"type":"object","additionalProperties":false,"properties":{
"action":{"type":"string","enum":["add","replace","merge","remove"],"description":"'add': propose a new rule; 'replace': update the rule with the given id; 'merge': replace the rules at the given ids with one consolidated rule; 'remove': disable the rule at the given id"},
"id":{"type":"integer","description":"Target rule ID (required for replace/remove)"},
"ids":{"type":"array","items":{"type":"integer"},"description":"Rule IDs to merge into one (required for merge, at least 2)"},
"text":{"type":"string","maxLength":%d,"description":"New rule text, short imperative (required for add/replace/merge), e.g. 'Reply only in Russian to this user'"},
"reason":{"type":"string","maxLength":%d,"description":"Evidence from the conversation that justifies this operation"}},"required":["action","reason"]}}},
"required":["operations"]}`, storage.CorrectionTextLimit, storage.CorrectionTextLimit)),
}}

func reviewOf(r turnResult) ([]agent.ReviewOperation, error) {
	if r.Name != toolReviewDone {
		return nil, errors.New("reviewer did not call review_done")
	}
	var args struct {
		Operations []agent.ReviewOperation `json:"operations"`
	}
	if json.Unmarshal([]byte(r.Arguments), &args) != nil {
		return nil, errors.New("reviewer returned invalid arguments")
	}
	return args.Operations, nil
}

func summaryOf(r turnResult) (string, error) {
	if r.Name != toolSummary {
		if text := strings.TrimSpace(r.Prose); text != "" {
			return text, nil
		}
		return "", errors.New("model returned no summary")
	}
	var args struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal([]byte(r.Arguments), &args) != nil || strings.TrimSpace(args.Summary) == "" {
		return "", errors.New("model returned invalid compaction arguments")
	}
	return args.Summary, nil
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

func (e *Engine) proposeCorrections(ctx context.Context, job queue.Job, ops []agent.ReviewOperation) error {
	return e.DB.Agent().ApplyReviewOperations(ctx, job.Scope, ops)
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
			fmt.Fprintf(&out, "#%d [%s] %s\n", c.ID, c.Status, c.Text)
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
