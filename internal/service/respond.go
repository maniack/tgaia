package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/queue"
)

// Terminal tools end a run. Their descriptions say what the call does, like
// any tool contract; how and when the bot answers is up to its prompt.
const (
	toolReply      = "reply"
	toolSkip       = "skip"
	toolUserChoice = "user_choice"
)

// UserChoiceArgs holds the parsed arguments of a user_choice terminal call.
type UserChoiceArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Multi    bool     `json:"multi"`
}

var (
	replyTool      = llm.Tool{Type: "function", Function: llm.ToolFunction{Name: toolReply, Description: "Send your answer to the current event to the chat and end the turn. The text is delivered exactly as written.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"text":{"type":"string","description":"The complete message for the chat."}},"required":["text"]}`)}}
	skipTool       = llm.Tool{Type: "function", Function: llm.ToolFunction{Name: toolSkip, Description: "End the turn without sending anything. Offered only when the current message is not addressed to you.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{}}`)}}
	userChoiceTool = llm.Tool{Type: "function", Function: llm.ToolFunction{Name: toolUserChoice, Description: "Ask the user to choose from a list of options you define, using Telegram inline buttons. Calling this ends your turn — the user's selection triggers the next turn. Use only when the user asked for a choice or when narrowing their intent requires a pick. Include enough context in the question that the user knows what they're answering. Never call this for yes/no questions — just ask in text.", Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"question":{"type":"string","maxLength":500,"description":"Question shown above the buttons."},"options":{"type":"array","items":{"type":"string","maxLength":100},"minItems":1,"maxItems":10,"description":"Button labels."},"multi":{"type":"boolean","description":"true = multi-select with a Done button; false = single-select."}},"required":["question","options","multi"]}`)}}
)

// systemPrompt is the single prompt of a bot: its own text from the database,
// or the operator's default file. Code adds data after it, never instructions.
func (e *Engine) systemPrompt(ctx context.Context, scope chat.Scope) (string, error) {
	prompt, err := e.DB.Bots().Prompt(ctx, scope.AgentID, scope.BotID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(prompt) == "" {
		return readDefaultPrompt(e.DefaultPromptFile, e.Config.Limit("system_prompt_bytes"))
	}
	return prompt, nil
}

// responseEvent describes what the bot is reacting to, as data.
func (e *Engine) responseEvent(job queue.Job, payload agent.JobPayload, history agent.Context, profile *agent.TelegramProfile, requestedAt time.Time, rewrite bool) string {
	event := map[string]any{}
	if payload.Prompt != "" {
		event["type"] = "scheduled_task_due"
		event["task"] = payload.Prompt
		event["task_id"] = payload.TaskID
		event["owner"] = senderOf(job, history, payload.MessageID, profile)
		if payload.DueAt > 0 {
			event["due_at"] = time.UnixMilli(payload.DueAt).UTC().Format(time.RFC3339)
		}
		if payload.IntervalMS > 0 {
			event["repeats_every_seconds"] = payload.IntervalMS / 1000
		}
	} else {
		event["type"] = "message"
		if rewrite {
			event["type"] = "message_edited"
			event["delivery"] = "replaces your earlier answer to this message"
		}
		event["message_id"] = payload.MessageID
		event["from"] = senderOf(job, history, payload.MessageID, profile)
		event["addressed_to_you"] = payload.Addressed
		if !requestedAt.IsZero() {
			event["sent_at"] = requestedAt.UTC().Format(time.RFC3339)
		}
	}
	data, _ := json.Marshal(event)
	return string(data) + "\n"
}

func senderOf(job queue.Job, history agent.Context, messageID int64, profile *agent.TelegramProfile) string {
	if job.SenderChatID != 0 {
		return "sender_chat"
	}
	for _, m := range history.Messages {
		if m.MessageID == messageID && m.UserID == job.ActorUserID {
			return m.Who()
		}
	}
	if profile != nil {
		return agent.Message{UserID: profile.UserID, Name: profile.Name, Username: profile.Username}.Who()
	}
	return ""
}

// offerActions adds the built-in and MCP tools this sender may use, with the
// round and call limits that apply in this chat.
func (e *Engine) offerActions(ctx context.Context, t *turn, payload agent.JobPayload, requestedAt time.Time, behavior agent.ChatBehavior) error {
	if _, ok := e.Model.(toolModel); !ok {
		return nil
	}
	now := requestedAt
	if now.IsZero() {
		now = time.Now()
		if e.Now != nil {
			now = e.Now()
		}
	}
	builtin := &builtinTools{engine: e, job: t.job, payload: payload, now: now, scheduled: payload.Prompt != ""}
	if e.Config.TelegramAdminTools && !builtin.scheduled {
		if moderator, ok := e.Bots[t.job.Scope.BotID].(moderationSender); ok {
			if permissions, err := moderator.ModerationPermissions(ctx, t.job.Scope.ChatID, t.job.Scope.BotID); err == nil {
				builtin.moderation = permissions
			}
		}
	}
	t.builtin = builtin
	t.actions = builtin.specs()
	if e.Tools != nil {
		session, err := e.Tools.Open(ctx, t.job.Scope)
		if err != nil {
			return queue.WithPhase(err, "tool_open")
		}
		t.session = session
		t.actions = append(t.actions, session.Tools()...)
	}
	botRounds, botCalls, err := e.DB.Bots().ToolLimits(ctx, t.job.Scope.AgentID, t.job.Scope.BotID)
	if err != nil {
		return err
	}
	t.rounds = capped(e.Config.Limit("tool_rounds"), tighter(behavior.ChatToolRounds, botRounds))
	t.calls = capped(e.Config.Limit("tool_calls"), tighter(behavior.ChatToolCalls, botCalls))
	return nil
}

// capped applies an optional lower cap (0 = none) to a global limit.
func capped(global, local int) int {
	if local > 0 && local < global {
		return local
	}
	return global
}

// replyOf turns the run's terminal call into the stored response.
func replyOf(r turnResult) (Response, error) {
	switch {
	case r.Name == toolSkip:
		return Response{Skip: true}, nil
	case r.Name == toolUserChoice:
		var args UserChoiceArgs
		if json.Unmarshal([]byte(r.Arguments), &args) != nil || len(args.Options) == 0 || len(args.Options) > 10 || strings.TrimSpace(args.Question) == "" {
			return Response{}, errors.New("invalid user_choice arguments")
		}
		for _, opt := range args.Options {
			if strings.TrimSpace(opt) == "" || len([]rune(opt)) > 100 {
				return Response{}, errors.New("invalid user_choice option")
			}
		}
		return Response{UserChoice: &args}, nil
	case r.Name == toolReply:
		var args struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(r.Arguments), &args) != nil {
			return Response{}, errors.New("invalid reply arguments")
		}
		r.Prose = args.Text
	}
	text := strings.TrimSpace(r.Prose)
	if text == "" || len(text) > replyLimit {
		return Response{}, errors.New("model reply size invalid")
	}
	return Response{Reply: text}, nil
}
