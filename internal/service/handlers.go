package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/maniack/tgaia/internal/access"
	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/messages"
	"github.com/maniack/tgaia/internal/queue"
	"github.com/maniack/tgaia/internal/storage"
	"github.com/maniack/tgaia/internal/telemetry"
)

type Model interface {
	Complete(context.Context, []llm.Message) (string, error)
}
type MemoryEmbedder interface {
	Embed(context.Context, string) ([]float32, error)
	Model() string
}
type Sender interface {
	Send(context.Context, int64, string) (int64, time.Time, error)
	Edit(context.Context, int64, int64, string) error
	Delete(context.Context, int64, int64) error
}
type BotIdentity struct {
	Name, Username, Bio, Description, AvatarDescription string
	AvatarPresent                                       bool
	AvatarUniqueID                                      string
	ProfileRefreshedAt                                  time.Time
}
type Engine struct {
	Config             config.App
	Identities         map[int64]BotIdentity
	Now                func() time.Time
	DefaultPromptFile  string
	ReviewerPromptFile string
	Tools              ToolProvider
	DB                 *storage.Database
	Model              Model
	Embedder           MemoryEmbedder
	Bots               map[int64]Sender
	Log                *slog.Logger
}

func (e *Engine) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *Engine) botName(botID int64) string {
	for _, b := range e.Config.Bots {
		if b.ID == botID {
			return b.Name
		}
	}
	return ""
}

func (e *Engine) recordRunMetrics(ctx context.Context, r turnResult) {
	jm := telemetry.JobMetricsFromContext(ctx)
	if jm == nil {
		return
	}
	jm.Rounds = int64(r.Rounds)
	jm.DeniedCalls = int64(r.DeniedCalls)
	terminal := r.Name
	if terminal == "" && r.Prose != "" {
		terminal = "prose"
	}
	jm.Terminal = terminal
}

// Response is the model's final envelope. Everything else the bot does
// (tasks, memory) happens through tools during the tool phase, so the final
// call can only write text.
type Response struct {
	Reply      string          `json:"reply"`
	Skip       bool            `json:"skip,omitempty"`
	UserChoice *UserChoiceArgs `json:"-"` // in-memory only; not stored
}

// storedResult is the durable per-job outcome: the reply plus the scheduler's
// own description of any task created in this job. The footer built from it
// is code-generated, so the user always sees the time that was stored.
type storedResult struct {
	Reply         string   `json:"reply"`
	Skip          bool     `json:"skip,omitempty"`
	UserChoice    bool     `json:"user_choice,omitempty"`
	Confirmations []string `json:"confirmations,omitempty"`
}

// ChoiceSender can send inline-keyboard messages and answer callbacks.
type ChoiceSender interface {
	SendButtons(ctx context.Context, chatID, threadID int64, text string, keyboard [][]agent.InlineButton) (int64, error)
}

func (r storedResult) text() string {
	if len(r.Confirmations) == 0 {
		return r.Reply
	}
	if r.Reply == "" {
		return "⏰ " + strings.Join(r.Confirmations, "\n⏰ ")
	}
	return r.Reply + "\n\n⏰ " + strings.Join(r.Confirmations, "\n⏰ ")
}

// tighter combines two optional caps where zero means "not set".
func tighter(a, b int) int {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

// replyLimit bounds a stored reply. Long answers are delivered as documents, so
// the limit is a safety ceiling rather than a Telegram message size.
const replyLimit = 64000

func (e *Engine) Handlers() map[string]queue.Handler {
	return map[string]queue.Handler{"respond": e.Respond, "compact": e.Compact, "review_behavior": e.ReviewBehavior, "message_action": e.MessageAction, "media": e.Describe, "failure_notice": e.FailureNotice}
}
func modelContext(c agent.Context, budgets ...int) string {
	budget := 100000
	if len(budgets) > 0 {
		budget = budgets[0]
	}
	header := "## Shared knowledge\n" + c.Knowledge + "\n## Saved memories of this chat\n" + explicitMemoryContext(c.ExplicitMemories, 16000) + "\n## Reactions to your messages\n" + reactionContext(c.Reactions, 12000) + "\n## Conversation summary\n" + c.Summary.Content + "\n## Conversation\n"
	lines := []string{}
	size := len(header)
	for i := len(c.Messages) - 1; i >= 0; i-- {
		m := c.Messages[i]
		line := promptMessage(m)
		if size+len(line) > budget {
			break
		}
		lines = append(lines, line)
		size += len(line)
	}
	var out strings.Builder
	out.WriteString(header)
	for i := len(lines) - 1; i >= 0; i-- {
		out.WriteString(lines[i])
	}
	return out.String()
}

func reactionContext(reactions []agent.MessageReaction, budget int) string {
	var out strings.Builder
	for _, reaction := range reactions {
		actor := reaction.ActorName
		if reaction.ActorUsername != "" {
			if actor != "" {
				actor += " "
			}
			actor += "@" + reaction.ActorUsername
		}
		if actor == "" {
			if reaction.ActorType == "anonymous" {
				actor = "anonymous participants"
			} else {
				actor = fmt.Sprintf("%s:%d", reaction.ActorType, reaction.ActorID)
			}
		}
		if reaction.ActorIsBot {
			actor += " (bot)"
		}
		value := reaction.Value
		if reaction.Type == "custom_emoji" {
			value = "custom_emoji:" + value
		} else if reaction.Type == "paid" && value == "" {
			value = "paid reaction"
		} else if value == "" {
			value = reaction.Type
		}
		line := fmt.Sprintf("[bot_message_id=%d actor=%q reaction=%q count=%d] bot_message=%q\n", reaction.MessageID, actor, value, reaction.Count, reaction.MessageText)
		if out.Len()+len(line) > budget {
			break
		}
		out.WriteString(line)
	}
	if out.Len() == 0 {
		return "None.\n"
	}
	return out.String()
}

// promptMessage keeps derived media text attached to its source message. The
// label matters: OCR and image descriptions are useful conversation data, but
// must never look like a separate user instruction to the model.
func promptMessage(m agent.Message) string {
	line := m.Header() + " " + m.Text + "\n"
	if media := strings.TrimSpace(m.MediaText); media != "" {
		line += "[attachment description: " + media + "]\n"
	}
	return line
}

func explicitMemoryContext(memories []agent.ExplicitMemory, budget int) string {
	lines := make([]string, 0, len(memories))
	size := 0
	for _, memory := range memories {
		line := fmt.Sprintf("[saved_by=%s source_message_id=%d] %q\n", memory.Author(), memory.SourceMessageID, memory.Content)
		if size+len(line) > budget {
			break
		}
		lines = append(lines, line)
		size += len(line)
	}
	return strings.Join(lines, "")
}

func senderProfileContext(profile agent.TelegramProfile, memories []agent.ProfileMemory, budget int) string {
	metadata, _ := json.Marshal(map[string]any{"user_id": profile.UserID, "name": profile.Name, "username": profile.Username, "language_code": profile.LanguageCode, "telegram_premium": profile.IsPremium, "is_bot": profile.IsBot})
	header := "## Sender profile\nTelegram: " + string(metadata) + "\nRemembered facts:\n"
	var out strings.Builder
	out.WriteString(header)
	for _, memory := range memories {
		line := fmt.Sprintf("[source_chat=%q source_message_id=%d] %q\n", memory.ChatTitle, memory.SourceMessageID, memory.Content)
		if out.Len()+len(line) > budget {
			break
		}
		out.WriteString(line)
	}
	return out.String()
}

func mergeExplicitMemories(relevant, recent []agent.ExplicitMemory) []agent.ExplicitMemory {
	result := make([]agent.ExplicitMemory, 0, len(relevant)+len(recent))
	seen := map[int64]bool{}
	for _, memories := range [][]agent.ExplicitMemory{relevant, recent} {
		for _, memory := range memories {
			if !seen[memory.SourceMessageID] {
				seen[memory.SourceMessageID] = true
				result = append(result, memory)
			}
		}
	}
	return result
}

func (e *Engine) Respond(ctx context.Context, job queue.Job) error {
	start := time.Now()
	var runMeta turnResult
	store := e.DB.Agent()
	if err := store.Check(ctx, job.Scope, job.ActorUserID); err != nil {
		return err
	}
	sender := e.Bots[job.Scope.BotID]
	if sender == nil {
		return errors.New("bot sender unavailable")
	}
	var payload agent.JobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return errors.New("invalid response job")
	}
	actionErr := store.CheckActions(ctx, job.Scope, job.ActorUserID)
	reader := job.SenderChatID != 0 || errors.Is(actionErr, access.ErrDenied)
	if actionErr != nil && !errors.Is(actionErr, access.ErrDenied) {
		return actionErr
	}
	if payload.Prompt != "" && reader {
		return nil
	}
	current, err := store.ResponseCurrent(ctx, job.Scope, payload.MessageID, payload.SourceUpdateID)
	if err != nil {
		return err
	}
	if !current {
		return nil
	}
	var prior []storage.ReplyLink
	if payload.Edited && payload.MessageID > 0 {
		prior, err = store.ReplyLinks(ctx, job.Scope, payload.MessageID)
		if err != nil {
			return err
		}
		// If the prior reply links exist but the original respond job never
		// succeeded (only failure_notice was sent), treat the edit as a fresh
		// request with tools so the user gets a real answer.
		if len(prior) > 0 {
			ok, checkErr := store.RespondSucceeded(ctx, job.Scope, payload.MessageID)
			if checkErr != nil {
				return checkErr
			}
			if !ok {
				prior = nil
			}
		}
	}
	rewrite := len(prior) > 0
	behavior, err := store.ChatBehavior(ctx, job.Scope)
	if err != nil {
		return err
	}
	stored, err := store.Result(ctx, job.ID)
	if err != nil {
		return err
	}
	if stored == "" {
		stopTyping := e.startTyping(ctx, job, sender)
		defer stopTyping()
		var history agent.Context
		var requestedAt time.Time
		var choiceSessionID int64
		if payload.ChoiceSessionID > 0 {
			sess, sessErr := store.GetChoiceSession(ctx, payload.ChoiceSessionID)
			if sessErr != nil {
				return sessErr
			}
			if sess == nil {
				// Session already consumed (concurrent job or retry); nothing to do.
				return nil
			}
			history, err = store.Context(ctx, job.Scope, sess.ContextCursor, e.Config.Limit("reply_messages"))
			if err != nil {
				return err
			}
			history.Messages = append(history.Messages,
				agent.Message{Role: "assistant", Text: agent.ChoiceAssistantText(sess)},
				agent.Message{Role: "user", Text: agent.ChoiceUserText(sess), UserID: job.ActorUserID},
			)
			requestedAt = time.Now()
			choiceSessionID = payload.ChoiceSessionID
		} else {
			history, requestedAt, err = store.ReplyContext(ctx, job.Scope, payload.MessageID, e.Config.Limit("reply_messages"))
			if err != nil {
				return err
			}
		}
		if err := e.enrichMedia(ctx, job, &history, sender); err != nil {
			return err
		}
		if e.Embedder != nil && len(history.ExplicitMemories) > 0 {
			query := ""
			for i := len(history.Messages) - 1; i >= 0; i-- {
				if history.Messages[i].MessageID == payload.MessageID {
					query = history.Messages[i].Text
					break
				}
			}
			if query != "" {
				if embedding, embedErr := e.Embedder.Embed(ctx, query); embedErr == nil {
					relevant, searchErr := store.SearchExplicitMemories(ctx, job.Scope, e.Embedder.Model(), embedding, 20)
					if searchErr != nil {
						return searchErr
					}
					history.ExplicitMemories = mergeExplicitMemories(relevant, history.ExplicitMemories)
				}
			}
		}
		system, err := e.systemPrompt(ctx, job.Scope)
		if err != nil {
			return err
		}
		var profile *agent.TelegramProfile
		var profileMemories []agent.ProfileMemory
		if job.ActorUserID > 0 {
			value, profileErr := store.TelegramProfile(ctx, job.Scope.AgentID, job.ActorUserID)
			if profileErr != nil {
				return profileErr
			}
			profile = &value
			profileMemories, profileErr = store.UserProfileMemories(ctx, job.Scope.AgentID, job.ActorUserID)
			if profileErr != nil {
				return profileErr
			}
		}
		// Skipping is for messages the bot merely overhears (group_response=all).
		// A private message, a mention or a reply to the bot always gets an answer.
		allowSkip := behavior.AllowSkip && !payload.Addressed && !rewrite
		input := modelContext(history, e.Config.Limit("reply_context_bytes"))
		if profile != nil {
			input += senderProfileContext(*profile, profileMemories, 16000)
		}
		input += "\n## Event\n" + e.responseEvent(job, payload, history, profile, requestedAt, rewrite)
		corrections, err := e.correctionsContext(ctx, job.Scope, false)
		if err != nil {
			return err
		}
		t := turn{job: job, messages: []llm.Message{{Role: "system", Content: system + e.runtimeContext(job.Scope.BotID) + corrections}, {Role: "user", Content: input}}, terminal: []llm.Tool{replyTool}, checkpoint: true, budget: e.Config.Limit("tool_output_tokens")}
		if allowSkip {
			t.terminal = append(t.terminal, skipTool)
		}
		// What the bot can do here is expressed by the tools it is offered:
		// a reader, a sender chat or a revision of an earlier answer gets none.
		runCtx, cancelRun := context.WithTimeout(ctx, e.Config.Duration("tool_timeout"))
		defer cancelRun()
		if !reader && !rewrite {
			if err := e.offerActions(runCtx, &t, payload, requestedAt, behavior); err != nil {
				return err
			}
			if t.session != nil {
				defer t.session.Close()
			}
			// Offer user_choice when the bot can send inline keyboards.
			if _, ok := e.Bots[job.Scope.BotID].(ChoiceSender); ok && payload.ChoiceSessionID == 0 {
				t.terminal = append(t.terminal, userChoiceTool)
			}
		}
		outcome, err := e.run(runCtx, t)
		if err != nil {
			return err
		}
		runMeta = outcome
		e.recordRunMetrics(ctx, outcome)
		response, err := replyOf(outcome)
		if err != nil {
			return queue.WithPhase(err, "final")
		}
		if response.Skip && len(outcome.Confirmations) > 0 {
			// Silence must not hide a task the scheduler just stored.
			response.Skip = false
		}
		if response.UserChoice != nil {
			// LLM chose to ask the user a question via inline buttons.
			choiceSender := e.Bots[job.Scope.BotID].(ChoiceSender)
			cursor, cursorErr := store.MaxConversationID(ctx, job.Scope)
			if cursorErr != nil {
				return cursorErr
			}
			sess := agent.ChoiceSession{
				AgentID: job.Scope.AgentID, BotID: job.Scope.BotID,
				ChatID: job.Scope.ChatID, ThreadID: job.Scope.ThreadID,
				UserID:        job.ActorUserID,
				Question:      response.UserChoice.Question,
				Options:       response.UserChoice.Options,
				Multi:         response.UserChoice.Multi,
				ContextCursor: cursor,
			}
			sessID, createErr := store.CreateChoiceSession(ctx, sess)
			if createErr != nil {
				return createErr
			}
			keyboard := agent.BuildChoiceKeyboard(sessID, response.UserChoice.Options, response.UserChoice.Multi, nil)
			msgID, sendErr := choiceSender.SendButtons(runCtx, job.Scope.ChatID, job.Scope.ThreadID, response.UserChoice.Question, keyboard)
			if sendErr != nil {
				_ = store.DeleteChoiceSession(ctx, sessID)
				return sendErr
			}
			_ = store.SetChoiceMessageID(ctx, sessID, msgID)
			data, _ := json.Marshal(storedResult{UserChoice: true})
			stored = string(data)
			stopTyping()
			if err := store.SaveResult(ctx, job, stored); err != nil {
				return err
			}
		} else {
			data, _ := json.Marshal(storedResult{Reply: response.Reply, Skip: response.Skip, Confirmations: outcome.Confirmations})
			stored = string(data)
			stopTyping()
			if err := store.SaveResult(ctx, job, stored); err != nil {
				return err
			}
		}
		if choiceSessionID > 0 {
			_ = store.DeleteChoiceSession(ctx, choiceSessionID)
		}
	}
	var result storedResult
	if err := json.Unmarshal([]byte(stored), &result); err != nil || (!result.Skip && !result.UserChoice && result.Reply == "" && len(result.Confirmations) == 0) {
		return errors.New("invalid stored result")
	}
	if result.Skip || result.UserChoice {
		terminal := "skip"
		if result.UserChoice {
			terminal = "user_choice"
		}
		e.log().Info("job finished",
			"kind", job.Kind, "bot", e.botName(job.Scope.BotID),
			"chat_id", job.Scope.ChatID, "attempt", job.Attempt,
			"status", "completed", "terminal", terminal,
			"rounds", runMeta.Rounds, "tool_calls", runMeta.ToolCalls, "denied_calls", runMeta.DeniedCalls,
			"duration_ms", time.Since(start).Milliseconds(),
		)
		return nil
	}
	text := result.text()
	_, documents := sender.(DocumentSender)
	pieces := plan(text, documents)
	fallback, err := e.deliver(ctx, job, payload, sender, prior, rewrite, pieces)
	if err != nil {
		return err
	}
	if fallback {
		// Telegram refused the document (for example, no media permission in the
		// group). Nothing was sent, so multipart text is delivered instead.
		pieces = plan(text, false)
		if _, err := e.deliver(ctx, job, payload, sender, prior, rewrite, pieces); err != nil {
			return err
		}
	}
	terminal := runMeta.Name
	if terminal == "" && runMeta.Prose != "" {
		terminal = "prose"
	}
	e.log().Info("job finished",
		"kind", job.Kind, "bot", e.botName(job.Scope.BotID),
		"chat_id", job.Scope.ChatID, "attempt", job.Attempt,
		"status", "completed", "terminal", terminal,
		"rounds", runMeta.Rounds, "tool_calls", runMeta.ToolCalls, "denied_calls", runMeta.DeniedCalls,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

// deliver sends the planned pieces with per-part idempotency. In a rewrite,
// text parts edit the previous messages in place; a document replaces them,
// because a media message cannot be turned into text or back by editing.
// It reports fallback when a document was definitely rejected before sending.
func (e *Engine) deliver(ctx context.Context, job queue.Job, payload agent.JobPayload, sender Sender, prior []storage.ReplyLink, rewrite bool, pieces []piece) (bool, error) {
	store := e.DB.Agent()
	replaceAll := rewrite && len(pieces) == 1 && pieces[0].document
	// If newer user messages arrived while we were thinking, quote the triggering
	// message on the first part so the reply stays anchored in the conversation.
	replier, canReply := sender.(interface {
		SendReply(context.Context, int64, int64, int64, string) (int64, time.Time, error)
	})
	useReply := canReply && !rewrite && payload.MessageID > 0 && payload.Action != "failure_notice"
	if useReply {
		if has, _ := store.HasNewerUserMessage(ctx, job.Scope, payload.MessageID); !has {
			useReply = false
		}
	}
	for part, item := range pieces {
		if err := store.Check(ctx, job.Scope, job.ActorUserID); err != nil {
			return false, err
		}
		current, err := store.ResponseCurrent(ctx, job.Scope, payload.MessageID, payload.SourceUpdateID)
		if err != nil {
			return false, err
		}
		if !current {
			return false, nil
		}
		if rewrite && !replaceAll && !item.document {
			var target int64
			for _, ref := range prior {
				if ref.Part == part {
					target = ref.MessageID
				}
			}
			if target > 0 {
				action, _ := json.Marshal(agent.JobPayload{Action: "edit", MessageID: target, Text: item.text})
				mutation := job
				mutation.Payload = action
				if err := e.MessageAction(ctx, mutation); err != nil {
					return false, err
				}
				continue
			}
		}
		// The durable receipt keeps the full text; for a document that is the
		// whole reply, so history and the admin view stay complete.
		recorded := item.text
		if item.document {
			recorded = string(item.data)
			if item.file != "reply.md" && strings.TrimSpace(item.text) != "" {
				recorded = item.text + "\n\n```\n" + string(item.data) + "```"
			}
		}
		fresh, err := store.BeginSend(ctx, job, part, recorded)
		if err != nil {
			return false, err
		}
		if !fresh {
			state, err := store.DeliveryState(ctx, job.ID, part)
			if err != nil {
				return false, err
			}
			if state == "sent" || state == "dismissed" {
				continue
			}
			if err := store.MarkUncertain(ctx, job.ID, part); err != nil {
				return false, err
			}
			return false, nil
		}
		var id int64
		var at time.Time
		if item.document {
			id, at, err = sender.(DocumentSender).SendDocument(ctx, job.Scope.ChatID, job.Scope.ThreadID, item.file, item.data, item.text)
			var rejected Rejected
			if err != nil && errors.As(err, &rejected) && rejected.Rejected() {
				if err := store.AbandonSend(ctx, job, part); err != nil {
					return false, err
				}
				return true, nil
			}
		} else if payload.Action == "failure_notice" && payload.MessageID > 0 {
			if replier, ok := sender.(interface {
				SendReply(context.Context, int64, int64, int64, string) (int64, time.Time, error)
			}); ok {
				id, at, err = replier.SendReply(ctx, job.Scope.ChatID, job.Scope.ThreadID, payload.MessageID, item.text)
			} else if topic, ok := sender.(interface {
				SendTopic(context.Context, int64, int64, string) (int64, time.Time, error)
			}); ok {
				id, at, err = topic.SendTopic(ctx, job.Scope.ChatID, job.Scope.ThreadID, item.text)
			} else if job.Scope.ThreadID != 0 {
				return false, errors.New("sender does not support forum topics")
			} else {
				id, at, err = sender.Send(ctx, job.Scope.ChatID, item.text)
			}
		} else if useReply && part == 0 {
			id, at, err = replier.SendReply(ctx, job.Scope.ChatID, job.Scope.ThreadID, payload.MessageID, item.text)
		} else if topic, ok := sender.(interface {
			SendTopic(context.Context, int64, int64, string) (int64, time.Time, error)
		}); ok {
			id, at, err = topic.SendTopic(ctx, job.Scope.ChatID, job.Scope.ThreadID, item.text)
		} else if job.Scope.ThreadID != 0 {
			return false, errors.New("sender does not support forum topics")
		} else {
			id, at, err = sender.Send(ctx, job.Scope.ChatID, item.text)
		}
		if err != nil {
			e.log().Warn("delivery failed",
				"kind", job.Kind, "bot", e.botName(job.Scope.BotID),
				"chat_id", job.Scope.ChatID, "part", part,
				slog.String("error", err.Error()),
			)
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = store.MarkUncertain(cleanup, job.ID, part)
			stop()
			return false, nil
		} // Never blindly duplicate an uncertain send.
		if err := store.Sent(ctx, job, part, recorded, id, at); err != nil {
			return false, err
		}
	}
	if rewrite {
		parts := len(pieces)
		if replaceAll {
			parts = 0
		}
		for _, ref := range prior {
			if ref.Part >= parts {
				current, err := store.ResponseCurrent(ctx, job.Scope, payload.MessageID, payload.SourceUpdateID)
				if err != nil {
					return false, err
				}
				if !current {
					return false, nil
				}
				action, _ := json.Marshal(agent.JobPayload{Action: "delete", MessageID: ref.MessageID})
				mutation := job
				mutation.Payload = action
				if err := e.MessageAction(ctx, mutation); err != nil {
					return false, err
				}
			}
		}
	}
	return false, nil
}

// failureNotices are short, content-free user messages for retired jobs.
var failureNotices = map[string]string{
	"llm_timeout":       "⚠️ Не успел подготовить ответ: генерация заняла слишком много времени. Попробуйте сформулировать запрос короче или разбить его на части.",
	"llm_429":           "⚠️ Модель сейчас перегружена или достигла лимита запросов. Попробуйте немного позже.",
	"llm_http_5xx":      "⚠️ Сервис модели временно недоступен. Попробуйте немного позже.",
	"llm_http_4xx":      "⚠️ Сервис модели отклонил запрос. Администратору нужно проверить модель и её настройки.",
	"llm_network":       "⚠️ Не удалось связаться с сервисом модели. Попробуйте немного позже.",
	"llm_truncated":     "⚠️ Ответ получился слишком длинным и был обрезан. Попробуйте разбить запрос на части.",
	"llm_invalid":       "⚠️ Модель вернула ответ в неподдерживаемом формате. Попробуйте переформулировать запрос.",
	"llm_budget":        "⚠️ Запрос вместе с контекстом слишком велик для модели. Попробуйте сформулировать короче.",
	"embedding_timeout": "⚠️ Не удалось вовремя обработать память разговора. Попробуйте немного позже.",
	"embedding_failed":  "⚠️ Сервис памяти временно недоступен. Попробуйте немного позже.",
	"mcp_timeout":       "⚠️ Внешний инструмент не ответил вовремя. Перед повтором проверьте, не выполнил ли он действие.",
	"mcp_failed":        "⚠️ Внешний инструмент завершился с ошибкой. Попробуйте позже или обратитесь к администратору.",
	"quota":             "⚠️ Исчерпан лимит обращений к модели для этого чата. Попробуйте позже.",
	"budget":            "⚠️ Запрос превысил допустимый объём или число операций. Попробуйте упростить его.",
	"job_timeout":       "⚠️ Обработка запроса заняла слишком много времени и была остановлена. Попробуйте ещё раз.",
	"panic":             "⚠️ Во время обработки произошла внутренняя ошибка. Администратор увидит её в статусе приложения.",
	"handler_error":     "⚠️ Во время обработки произошла внутренняя ошибка. Попробуйте ещё раз; если ошибка повторится, сообщите администратору.",
	"other":             "⚠️ Во время обработки произошла внутренняя ошибка. Попробуйте ещё раз; если ошибка повторится, сообщите администратору.",
}

const failureNotice = "⚠️ Не смог ответить на это сообщение из-за ошибки. Попробуйте ещё раз или сформулируйте запрос короче."

func failureNoticeText(class string, scheduled bool) string {
	text := failureNotices[class]
	if text == "" {
		text = failureNotice
	}
	if scheduled {
		return "⚠️ Запланированная задача не выполнена. " + strings.TrimPrefix(text, "⚠️ ")
	}
	return text
}

// Failed runs once after a respond job is retired as dead. It creates a
// separate durable delivery job so retries never re-run the failed model or
// tool work and an uncertain Telegram send is never blindly duplicated.
func (e *Engine) Failed(ctx context.Context, job queue.Job, class string) {
	if job.Kind != "respond" {
		return
	}
	var payload agent.JobPayload
	if json.Unmarshal(job.Payload, &payload) != nil || job.SenderChatID != 0 {
		return
	}
	payload.Action = "failure_notice"
	payload.FailureClass = class
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = e.DB.Queue().Enqueue(ctx, queue.Input{Scope: job.Scope, ActorUserID: job.ActorUserID, Key: fmt.Sprintf("failure:%d", job.ID), Kind: "failure_notice", Payload: encoded, MaxAttempts: 3})
}

func (e *Engine) FailureNotice(ctx context.Context, job queue.Job) error {
	var payload agent.JobPayload
	if json.Unmarshal(job.Payload, &payload) != nil || payload.Action != "failure_notice" {
		return queue.Fail("handler_error", true, errors.New("invalid failure notice"))
	}
	sender := e.Bots[job.Scope.BotID]
	if sender == nil {
		return queue.Fail("handler_error", true, errors.New("bot sender unavailable"))
	}
	_, err := e.deliver(ctx, job, payload, sender, nil, false, []piece{{text: failureNoticeText(payload.FailureClass, payload.Prompt != "")}})
	return err
}
func (e *Engine) Compact(ctx context.Context, job queue.Job) error {
	start := time.Now()
	if err := e.DB.Agent().Check(ctx, job.Scope, 0); err != nil {
		return err
	}
	var payload agent.JobPayload
	dec := json.NewDecoder(bytes.NewReader(job.Payload))
	if err := dec.Decode(&payload); err != nil || payload.ThroughID <= 0 {
		return errors.New("invalid compaction job")
	}
	revision, err := e.DB.Agent().MemoryRevision(ctx, job.Scope)
	if err != nil {
		return err
	}
	history, err := e.DB.Agent().Context(ctx, job.Scope, payload.ThroughID, e.Config.Limit("compaction_messages"))
	if err != nil {
		return err
	}
	if len(history.Messages) == 0 {
		return nil
	}
	text, through := compactionInput(history, e.Config.Limit("compaction_input_bytes"))
	if through <= history.Summary.ThroughID {
		return errors.New("compaction input exceeds budget")
	}
	system, err := e.systemPrompt(ctx, job.Scope)
	if err != nil {
		return err
	}
	noThinking := false
	corrections, err := e.correctionsContext(ctx, job.Scope, true)
	if err != nil {
		return err
	}
	compactionEvent := "\n## Event\n{\"type\":\"memory_compaction\"}\n" +
		"Write the summary in the same language the chat participants use. " +
		"Record past events with their date (e.g. '2026-11-10: alert fired'), not as current states. " +
		"Store explicit instructions from users as directives to yourself in imperative form.\n"
	outcome, err := e.run(ctx, turn{job: job, messages: []llm.Message{{Role: "system", Content: system + e.runtimeContext(job.Scope.BotID)}, {Role: "user", Content: text + corrections + compactionEvent}}, terminal: []llm.Tool{summaryTool(e.Config.Limit("compaction_summary_chars"))}, compaction: true, budget: e.Config.Limit("compaction_output_tokens"), thinking: &noThinking})
	if err != nil {
		return queue.WithPhase(err, "compact")
	}
	e.recordRunMetrics(ctx, outcome)
	summary, err := summaryOf(outcome)
	if err != nil {
		return queue.WithPhase(err, "compact")
	}
	summary = boundSummary(summary, e.Config.Limit("compaction_summary_chars"))
	compactionCount, err := e.DB.Agent().SaveSummary(ctx, job, history.Summary.ThroughID, through, summary, revision)
	if err != nil {
		return err
	}
	reviewEvery := e.Config.Limit("behavior_review_every")
	if reviewEvery <= 0 {
		reviewEvery = 3
	}
	if compactionCount%int64(reviewEvery) == 0 {
		key := fmt.Sprintf("review:%d", compactionCount)
		payload, _ := json.Marshal(agent.JobPayload{})
		if _, err := e.DB.Queue().Enqueue(ctx, queue.Input{Scope: job.Scope, Key: key, Kind: "review_behavior", Payload: payload, MaxAttempts: 2}); err != nil {
			e.log().Warn("failed to enqueue review_behavior", "chat_id", job.Scope.ChatID, slog.String("error", err.Error()))
		}
	}
	e.log().Info("job finished",
		"kind", job.Kind, "bot", e.botName(job.Scope.BotID),
		"chat_id", job.Scope.ChatID, "attempt", job.Attempt,
		"status", "completed", "terminal", outcome.Name,
		"rounds", outcome.Rounds, "tool_calls", outcome.ToolCalls, "denied_calls", outcome.DeniedCalls,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

// ReviewBehavior runs a neutral reviewer LLM over the chat's compaction summary
// and recent messages to propose behaviour corrections. It is triggered after
// every Nth successful compaction; the reviewer uses a separate prompt file so
// it is never the bot itself correcting itself.
func (e *Engine) ReviewBehavior(ctx context.Context, job queue.Job) error {
	start := time.Now()
	if err := e.DB.Agent().Check(ctx, job.Scope, 0); err != nil {
		return err
	}
	reviewerPrompt, err := readDefaultPrompt(e.ReviewerPromptFile, 8000)
	if err != nil {
		return queue.Fail("handler_error", true, fmt.Errorf("behavior reviewer prompt unavailable: %w", err))
	}
	history, err := e.DB.Agent().Context(ctx, job.Scope, 0, e.Config.Limit("compaction_messages"))
	if err != nil {
		return err
	}
	corrections, err := e.correctionsContext(ctx, job.Scope, true)
	if err != nil {
		return err
	}
	var userContent strings.Builder
	userContent.WriteString("## Conversation summary\n")
	userContent.WriteString(history.Summary.Content)
	userContent.WriteString("\n\n## Recent messages\n")
	for _, m := range history.Messages {
		userContent.WriteString(promptMessage(m))
	}
	if corrections != "" {
		userContent.WriteString(corrections)
	}
	userContent.WriteString("\n## Event\n{\"type\":\"behavior_review\"}\n")
	noThinking := false
	outcome, err := e.run(ctx, turn{
		job:        job,
		messages:   []llm.Message{{Role: "system", Content: reviewerPrompt}, {Role: "user", Content: userContent.String()}},
		terminal:   []llm.Tool{reviewTool},
		compaction: true,
		budget:     e.Config.Limit("compaction_output_tokens"),
		thinking:   &noThinking,
	})
	if err != nil {
		return queue.WithPhase(err, "review")
	}
	e.recordRunMetrics(ctx, outcome)
	proposals, err := reviewOf(outcome)
	if err != nil {
		return queue.WithPhase(err, "review")
	}
	if err := e.proposeCorrections(ctx, job, proposals); err != nil {
		return err
	}
	e.log().Info("job finished",
		"kind", job.Kind, "bot", e.botName(job.Scope.BotID),
		"chat_id", job.Scope.ChatID, "attempt", job.Attempt,
		"status", "completed", "terminal", outcome.Name,
		"rounds", outcome.Rounds, "tool_calls", outcome.ToolCalls, "denied_calls", outcome.DeniedCalls,
		"duration_ms", time.Since(start).Milliseconds(),
	)
	return nil
}

// boundSummary enforces the memory size the prompt asks for but models do not
// reliably honor. It cuts at a paragraph or line boundary so sections stay whole.
func boundSummary(summary string, limit int) string {
	runes := []rune(strings.TrimSpace(summary))
	if limit <= 0 || len(runes) <= limit {
		return string(runes)
	}
	cut := string(runes[:limit])
	for _, sep := range []string{"\n\n", "\n", ". "} {
		if i := strings.LastIndex(cut, sep); i > limit/2 {
			cut = cut[:i+len(strings.TrimRight(sep, " "))]
			break
		}
	}
	return strings.TrimSpace(cut)
}

func compactionInput(c agent.Context, budgets ...int) (string, int64) {
	budget := 90000
	if len(budgets) > 0 {
		budget = budgets[0]
	}
	var text strings.Builder
	text.WriteString("## Current memory\n" + c.Summary.Content +
		"\n## Reactions to your messages\n" + reactionContext(c.Reactions, 12000) +
		"## New messages\n")
	through := c.Summary.ThroughID
	for _, m := range c.Messages {
		line := promptMessage(m)
		if text.Len()+len(line) > budget {
			break
		}
		text.WriteString(line)
		through = m.ID
	}
	return text.String(), through
}

func (e *Engine) MessageAction(ctx context.Context, job queue.Job) error {
	if err := e.DB.Agent().CheckLease(ctx, job); err != nil {
		return err
	}
	if err := e.DB.Agent().Check(ctx, job.Scope, job.ActorUserID); err != nil {
		return err
	}
	var payload agent.JobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return errors.New("invalid message action")
	}
	ref := messages.Reference{Scope: job.Scope, MessageID: payload.MessageID}
	current, err := e.DB.BotMessages().Get(ctx, ref)
	if err != nil {
		return err
	}
	sender := e.Bots[job.Scope.BotID]
	if sender == nil {
		return errors.New("bot sender unavailable")
	}
	if current.DeletedAt != nil {
		if payload.Action == "delete" {
			return e.DB.Agent().SyncMessage(ctx, job.Scope, payload.MessageID, "", true)
		}
		return errors.New("message already deleted")
	}
	if payload.Action == "edit" && payload.Text != "" && len([]rune(payload.Text)) <= 3500 {
		if err := sender.Edit(ctx, job.Scope.ChatID, payload.MessageID, payload.Text); err != nil {
			return err
		}
		result, err := e.DB.BotMessages().ConfirmEdited(ctx, ref, current.Version, time.Now().UTC())
		if err != nil {
			return err
		}
		if result == messages.Conflict {
			return errors.New("message edit version conflict")
		}
		return e.DB.Agent().SyncMessage(ctx, job.Scope, payload.MessageID, payload.Text, false)
	}
	if payload.Action == "delete" {
		if err := sender.Delete(ctx, job.Scope.ChatID, payload.MessageID); err != nil {
			return err
		}
		result, err := e.DB.BotMessages().ConfirmDeleted(ctx, ref, current.Version, time.Now().UTC())
		if err != nil {
			return err
		}
		if result == messages.Conflict {
			return errors.New("message delete version conflict")
		}
		return e.DB.Agent().SyncMessage(ctx, job.Scope, payload.MessageID, "", true)
	}
	return errors.New("unsupported message action")
}

// runtimeContext is the data appended to the bot's prompt: who the bot is,
// the current time and what the platform can do.
func (e *Engine) runtimeContext(botID int64) string {
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	identity := e.Identities[botID]
	metadata, _ := json.Marshal(map[string]any{"bot_id": botID, "name": identity.Name, "username": identity.Username, "bio": identity.Bio, "description": identity.Description, "avatar_present": identity.AvatarPresent, "avatar_description": identity.AvatarDescription, "current_time": now.UTC().Format(time.RFC3339), "weekday": now.UTC().Weekday().String(), "timezone": "UTC"})
	return "\n\n## Runtime\n" + string(metadata) + e.capabilities(botID)
}

// capabilities is the trusted self-description, built from configuration.
// When asked how it works, the bot answers from this and nothing else: no
// product it has read about, no memory summary, no guess.
func (e *Engine) capabilities(botID int64) string {
	alias := ""
	for _, b := range e.Config.Bots {
		if b.ID == botID {
			alias = b.Name
		}
	}
	// Only servers this bot may actually open; naming others would be a false claim.
	servers := []string{}
	for _, s := range e.Config.MCPServers {
		if s.Enabled && alias != "" && (slices.Contains(s.Bots, "*") || slices.Contains(s.Bots, alias)) {
			servers = append(servers, s.Name)
		}
	}
	if len(servers) == 0 {
		servers = append(servers, "none for this bot")
	}
	manifest := map[string]any{
		"software":       "tgaia, an open-source Telegram agent written in Go (github.com/maniack/tgaia)",
		"model":          e.Config.LLMModel,
		"memory":         "per-chat conversation history, a periodically compacted long-term summary, and durable user facts with conservative automatic profile extraction (tools memory_list/memory_save/memory_update/memory_delete)",
		"scheduler":      "one-time and fixed-interval tasks per chat (tools task_list/task_create/task_update/task_delete); no calendar rules yet",
		"images":         "images in the chat are described by the model and the description is kept in history",
		"messages":       "current-chat bot messages can be listed, separately sent, edited and deleted through message_list/message_send/message_edit/message_delete",
		"skills":         "the agent can create, test, version, run and delete sandboxed Starlark skills stored in the database; skills have no host filesystem, network, process or environment access",
		"virtual_files":  "skills and tools can store text under scoped /user, /chat and /bot paths in the database; this is not the host filesystem",
		"moderation":     map[bool]string{true: "permission-gated Telegram group delete/ban/restrict/pin tools are enabled in configuration and still require live Telegram administrator rights", false: "Telegram group moderation tools are off by configuration"}[e.Config.TelegramAdminTools],
		"external_tools": "MCP servers the administrator enabled for this bot: " + strings.Join(servers, ", "),
		"not_available":  "no OAuth or account integrations (no Gmail, Notion, GitHub, Jira access unless an MCP server provides it), no audio or video recognition, no access to its own source code, configuration, secrets or infrastructure, no messages to other chats, no changes to permissions",
	}
	data, _ := json.Marshal(manifest)
	return "\n## Platform\n" + string(data)
}
