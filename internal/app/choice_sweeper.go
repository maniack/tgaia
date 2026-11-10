package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/storage"
	"github.com/maniack/tgaia/internal/telegram"
)

// handleCallback dispatches a callback result: answers the query and updates
// the keyboard when needed. client must correspond to botID.
func handleCallback(ctx context.Context, client *telegram.Client, agentID string, botID int64, cb agent.Callback, db *storage.Database, log *slog.Logger) {
	result, err := db.Agent().AcceptCallback(ctx, agentID, botID, cb)
	if err != nil || result.Action == "ignore" {
		return
	}
	toast := result.Toast
	if toast == "" && result.Action == "expired" {
		toast = "This question has expired."
	}
	if err := client.AnswerCallbackQuery(ctx, cb.QueryID, toast); err != nil && log != nil {
		log.Warn("answerCallbackQuery failed", "query_id", cb.QueryID)
	}
	switch result.Action {
	case "toggle":
		if result.Session == nil {
			return
		}
		keyboard := agent.BuildChoiceKeyboard(result.Session.ID, result.Session.Options, result.Session.Multi, result.Session.Selected)
		if err := client.EditMessageReplyMarkup(ctx, result.Session.ChatID, result.Session.MessageID, keyboard); err != nil && log != nil {
			log.Warn("editMessageReplyMarkup failed", "session_id", result.Session.ID)
		}
	case "done", "expired":
		if result.Session == nil {
			return
		}
		if err := client.EditMessageReplyMarkup(ctx, result.Session.ChatID, result.Session.MessageID, nil); err != nil && log != nil {
			log.Warn("editMessageReplyMarkup (clear) failed", "session_id", result.Session.ID)
		}
	}
}

// sweepChoices expires choice sessions that have passed their deadline, removes
// their keyboards, and runs every 60 seconds.
func sweepChoices(ctx context.Context, r *runtime) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current := r.current.Load()
			if current == nil {
				continue
			}
			expired, err := r.db.Agent().ExpiredChoiceSessions(ctx, 50)
			if err != nil {
				continue
			}
			for _, sess := range expired {
				client := clientForBot(current, sess.BotID)
				if client != nil {
					_ = client.EditMessageReplyMarkup(ctx, sess.ChatID, sess.MessageID, nil)
				}
				_ = r.db.Agent().DeleteChoiceSession(ctx, sess.ID)
			}
		}
	}
}

func clientForBot(snap *snapshot, botID int64) *telegram.Client {
	if snap == nil || snap.config.TelegramMode == "mock" {
		return nil
	}
	if c, ok := snap.pollers[botID]; ok {
		return c
	}
	// Webhook mode: pollers map is empty, build an on-the-fly client.
	for _, bot := range snap.config.Bots {
		if bot.ID == botID {
			return telegram.New(bot.Token)
		}
	}
	return nil
}

// callbackFromUpdate extracts a Callback from a Telegram Update if it carries
// a callback_query addressed to this bot; returns ok=false otherwise.
func callbackFromUpdate(update telegram.Update, agentID string, botID int64, bots []config.Bot) (agent.Callback, bool) {
	cq := update.CallbackQuery
	if cq == nil || cq.Message == nil {
		return agent.Callback{}, false
	}
	for _, b := range bots {
		if b.ID == botID {
			cb := agent.Callback{
				QueryID:   cq.ID,
				Data:      cq.Data,
				UserID:    cq.From.ID,
				Username:  cq.From.Username,
				ChatID:    cq.Message.Chat.ID,
				MessageID: cq.Message.ID,
			}
			return cb, true
		}
	}
	return agent.Callback{}, false
}
