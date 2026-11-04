package app

import (
	"context"
	"github.com/google/uuid"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/storage"
	"github.com/maniack/tgaia/internal/telegram"
	"log/slog"
	"time"
)

func poll(ctx context.Context, db *storage.Database, agentID string, bot config.Bot, client *telegram.Client, log *slog.Logger) {
	token := uuid.NewString()
	defer func() {
		release, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		_ = db.Agent().PollRelease(release, agentID, bot.ID, token)
	}()
	pause := func() bool {
		t := time.NewTimer(3 * time.Second)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	for ctx.Err() == nil {
		offset, owned, err := db.Agent().PollLease(ctx, agentID, bot.ID, token)
		if err != nil || !owned {
			if !pause() {
				return
			}
			continue
		}
		updates, err := client.Updates(ctx, offset)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("Telegram poll failed", "bot_id", bot.ID)
			}
			if !pause() {
				return
			}
			continue
		}
		for _, update := range updates {
			incoming, ok := telegram.Incoming(update, agentID, bot.ID, bot.Username)
			if ok {
				if err := db.Agent().Accept(ctx, incoming); err != nil {
					log.Warn("Telegram update persistence failed", "bot_id", bot.ID)
					break
				}
			}
			if err := db.Agent().PollAdvance(ctx, agentID, bot.ID, token, update.ID+1); err != nil {
				break
			}
		}
	}
}
