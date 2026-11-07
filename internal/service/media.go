package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/queue"
)

type mediaSource interface {
	Fetch(context.Context, agent.Attachment) ([]byte, string, error)
}

func (e *Engine) enrichMedia(ctx context.Context, job queue.Job, history *agent.Context, sender Sender) error {
	processed := 0
	// Recent attachments get priority. Keep bounded work even with a long backlog.
	for i := len(history.Messages) - 1; i >= 0; i-- {
		m := &history.Messages[i]
		if m.MediaText != "" {
			continue
		}
		var attachments []agent.Attachment
		if m.Attachments == "" {
			continue
		}
		if json.Unmarshal([]byte(m.Attachments), &attachments) != nil {
			return errors.New("invalid saved attachments")
		}
		if len(attachments) == 0 {
			continue
		}
		if processed >= e.Config.Limit("media_messages") {
			m.Text += "\n[Attachment not inspected: per-response limit]"
			continue
		}
		processed++
		var text strings.Builder
		for _, attachment := range attachments {
			if attachment.Kind == "audio" || attachment.Kind == "video" {
				text.WriteString("[" + attachment.Kind + ": not processed, recognition is disabled]\n")
				continue
			}
			if err := e.DB.Agent().Check(ctx, job.Scope, job.ActorUserID); err != nil {
				return err
			}
			source, ok := sender.(mediaSource)
			if !ok {
				text.WriteString("[attachment: download unavailable]\n")
				continue
			}
			data, mime, err := source.Fetch(ctx, attachment)
			if err != nil {
				text.WriteString("[attachment: could not be read (unsupported, oversized or unavailable)]\n")
				continue
			}
			value := string(data)
			if attachment.Kind == "image" {
				system, err := e.systemPrompt(ctx, job.Scope)
				if err != nil {
					return err
				}
				event, _ := json.Marshal(map[string]any{"type": "image_received", "message_id": m.MessageID, "from": m.Who(), "caption": m.Text})
				outcome, err := e.run(ctx, turn{job: job, messages: []llm.Message{{Role: "system", Content: system + e.runtimeContext(job.Scope.BotID)}, {Role: "user", Content: "## Event\n" + string(event) + "\n", Images: []string{"data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)}}}, terminal: []llm.Tool{describeTool}})
				if err != nil {
					return err
				}
				value = describedText(outcome)
			}
			if len(value) > 12000 {
				value = string([]rune(value)[:min(len([]rune(value)), 3000)]) + "\n[Attachment text truncated]"
			}
			if text.Len()+len(value) > 30000 {
				break
			}
			text.WriteString(value + "\n")
		}
		if err := e.DB.Agent().SaveMediaText(ctx, job, m.ID, m.UpdateID, text.String()); err != nil {
			return err
		}
		m.MediaText = text.String()
	}
	return nil
}

// Describe handles a "media" job: an image posted without addressing the bot.
// The description is stored in history for later replies and compaction; no
// reply is sent.
func (e *Engine) Describe(ctx context.Context, job queue.Job) error {
	store := e.DB.Agent()
	if err := store.Check(ctx, job.Scope, job.ActorUserID); err != nil {
		return err
	}
	var payload agent.JobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.MessageID <= 0 {
		return errors.New("invalid media job")
	}
	sender := e.Bots[job.Scope.BotID]
	if sender == nil {
		return errors.New("bot sender unavailable")
	}
	history, _, err := store.ReplyContext(ctx, job.Scope, payload.MessageID, 1)
	if err != nil {
		return err
	}
	// Only the target message: ReplyContext ends at it and limit 1 keeps it alone.
	for i := range history.Messages {
		if history.Messages[i].MessageID != payload.MessageID || history.Messages[i].UpdateID != payload.SourceUpdateID {
			history.Messages = append(history.Messages[:i], history.Messages[i+1:]...)
			break
		}
	}
	if len(history.Messages) == 0 || history.Messages[0].MediaText != "" {
		return nil
	}
	return e.enrichMedia(ctx, job, &history, sender)
}
