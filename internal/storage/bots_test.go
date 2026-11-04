package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/access"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/messages"
	"github.com/maniack/tgaia/internal/queue"
)

func TestBotsAndGlobalBlocks(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			db, other := queueDatabases(t, driver)
			ctx := context.Background()
			if applied, err := db.Bots().Register(ctx, "test", 4, "SECOND_BOT_TOKEN"); err != nil || !applied {
				t.Fatal("second bot registration failed")
			}
			if applied, err := other.Bots().Register(ctx, "test", 4, "SECOND_BOT_TOKEN"); err != nil || applied {
				t.Fatal("registration not idempotent")
			}
			if _, err := db.Bots().Register(ctx, "other", 4, "SECOND_BOT_TOKEN"); err == nil {
				t.Fatal("bot assigned to two agents")
			}
			if _, err := db.Bots().CredentialRef(ctx, "other", 4); err == nil {
				t.Fatal("cross-agent credential lookup")
			}
			if ref, err := db.Bots().CredentialRef(ctx, "test", 4); err != nil || ref != "SECOND_BOT_TOKEN" {
				t.Fatal("wrong credential routing")
			}
			if _, err := db.Bots().Register(ctx, "test", 5, "123:secret-token"); err == nil {
				t.Fatal("raw token accepted as reference")
			}

			scopes := []chat.Scope{{AgentID: "test", BotID: 1, ChatID: 10}, {AgentID: "test", BotID: 4, ChatID: 10}, {AgentID: "test", BotID: 4, ChatID: 11}}
			input := func(scope chat.Scope, key string, user int64) queue.Input {
				return queue.Input{Scope: scope, ActorUserID: user, Key: key, Kind: "test", Payload: []byte(`{}`), MaxAttempts: 3}
			}
			for _, scope := range scopes {
				if ok, err := db.Queue().Enqueue(ctx, input(scope, "same-update-id", 99)); err != nil || !ok {
					t.Fatal("bots share dedupe namespace")
				}
			}
			a := claim(t, db.Queue())
			b := claim(t, other.Queue())
			if a.Scope.ChatID != b.Scope.ChatID || a.Scope.BotID == b.Scope.BotID {
				t.Fatal("bots share serial execution scope")
			}
			if a.ActorUserID != 99 || b.ActorUserID != 99 {
				t.Fatal("actor identity lost")
			}
			if _, err := db.Access().SetUserBlocked(ctx, "test", 99, true); err != nil {
				t.Fatal(err)
			}
			if err := other.Access().CheckUser(ctx, "test", 99); !errors.Is(err, access.ErrBlocked) {
				t.Fatal("block invisible to other replica")
			}
			if err := other.Access().CheckUser(ctx, "other", 99); err != nil {
				t.Fatal("block leaked to separate agent")
			}
			for _, scope := range scopes {
				if _, err := other.Queue().Enqueue(ctx, input(scope, "blocked-new", 99)); !errors.Is(err, access.ErrBlocked) {
					t.Fatal("blocked user bypassed via another chat/bot")
				}
			}
			if err := other.Queue().Extend(ctx, a.Receipt, time.Minute); !errors.Is(err, queue.ErrLeaseLost) {
				t.Fatal("blocked actor retained live lease")
			}
			if err := other.Queue().Complete(ctx, b.Receipt); !errors.Is(err, queue.ErrLeaseLost) {
				t.Fatal("blocked actor completed live job")
			}
			if ok, err := other.Queue().Enqueue(ctx, input(scopes[0], "other-user", 100)); err != nil || !ok {
				t.Fatal("unblocked user denied")
			}
			survivor := claim(t, db.Queue())
			if survivor.ActorUserID != 100 {
				t.Fatal("blocked queued task executed")
			}
			if err := db.Queue().Complete(ctx, survivor.Receipt); err != nil {
				t.Fatal(err)
			}
			if job, err := other.Queue().Claim(ctx, []string{"test"}, time.Minute); err != nil || job != nil {
				t.Fatal("blocked work remained claimable")
			}
			if _, err := db.Access().SetUserBlocked(ctx, "test", 99, false); err != nil {
				t.Fatal(err)
			}
			if err := other.Access().CheckUser(ctx, "test", 99); err != nil {
				t.Fatal("unblock not effective")
			}
			if job, err := other.Queue().Claim(ctx, []string{"test"}, time.Minute); err != nil || job != nil {
				t.Fatal("unblock replayed suppressed work")
			}
			if _, err := db.Queue().Enqueue(ctx, input(scopes[0], "after-unblock", 99)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBotMessageIsolationWithinAgent(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			db, other := queueDatabases(t, driver)
			ctx := context.Background()
			if _, err := db.Bots().Register(ctx, "agent", 4, "SECOND_TOKEN"); err != nil {
				t.Fatal(err)
			}
			first := sentMessage(10, 100)
			second := first
			second.Scope.BotID = 4
			second.DeliveryKey = "second-bot"
			recordSent(t, db.BotMessages(), first)
			if _, err := other.BotMessages().Get(ctx, second.Reference); !errors.Is(err, messages.ErrNotFound) {
				t.Fatal("another bot could read message")
			}
			if result, err := other.BotMessages().ConfirmDeleted(ctx, second.Reference, 1, second.SentAt.Add(time.Minute)); !errors.Is(err, messages.ErrNotFound) || result != "" {
				t.Fatal("another bot could mutate message")
			}
			recordSent(t, other.BotMessages(), second)
			for _, sent := range []messages.Sent{first, second} {
				page, err := db.BotMessages().List(ctx, sent.Scope, "", 0, 10)
				if err != nil || len(page) != 1 || page[0].DeliveryKey != sent.DeliveryKey {
					t.Fatal("message list mixed bots")
				}
			}
			invalid := first
			invalid.Scope.BotID = 999
			if _, err := db.BotMessages().RecordSent(ctx, invalid); err == nil {
				t.Fatal("unregistered bot reference accepted")
			}
		})
	}
}
