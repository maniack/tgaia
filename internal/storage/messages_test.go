package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/messages"
)

func sentMessage(chatID, messageID int64) messages.Sent {
	return messages.Sent{Reference: messages.Reference{Scope: chat.Scope{AgentID: "agent", BotID: 2, ChatID: chatID}, MessageID: messageID}, DeliveryKey: "response-1", SentAt: time.Unix(1700000000, 0).UTC()}
}
func recordSent(t *testing.T, store *BotMessages, sent messages.Sent) {
	t.Helper()
	result, err := store.RecordSent(context.Background(), sent)
	if err != nil || result != messages.Applied {
		t.Fatalf("record sent: result=%s err=%v", result, err)
	}
}
func TestBotMessagesConformance(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			t.Run("references isolation and pagination", func(t *testing.T) {
				db, other := queueDatabases(t, driver)
				store, peer := db.BotMessages(), other.BotMessages()
				ctx := context.Background()
				first := sentMessage(1, 10)
				recordSent(t, store, first)
				result, err := peer.RecordSent(ctx, first)
				if err != nil || result != messages.Unchanged {
					t.Fatal("duplicate recording not idempotent")
				}
				changed := first
				changed.DeliveryKey = "different"
				result, err = peer.RecordSent(ctx, changed)
				if err != nil || result != messages.Conflict {
					t.Fatal("conflicting metadata overwritten")
				}
				second := sentMessage(1, 20)
				second.Part = 1
				recordSent(t, peer, second)
				duplicate := sentMessage(1, 30)
				duplicate.Part = 1
				recordSent(t, store, duplicate) // Same delivery/part, distinct actual send.
				recordSent(t, peer, sentMessage(2, 10))
				otherAgent := sentMessage(1, 10)
				otherAgent.Scope.AgentID = "other"
				otherAgent.Scope.BotID = 3
				recordSent(t, peer, otherAgent)
				page, err := store.List(ctx, first.Scope, first.DeliveryKey, 0, 2)
				if err != nil || len(page) != 2 || page[0].MessageID != 10 || page[1].MessageID != 20 {
					t.Fatal("first page invalid")
				}
				page, err = peer.List(ctx, first.Scope, first.DeliveryKey, page[1].MessageID, 2)
				if err != nil || len(page) != 1 || page[0].MessageID != 30 {
					t.Fatal("second page invalid")
				}
				ref := first.Reference
				ref.Scope.ChatID = 3
				if _, err := store.Get(ctx, ref); !errors.Is(err, messages.ErrNotFound) {
					t.Fatal("cross-chat lookup exposed a reference")
				}
				ref.Scope = chat.Scope{}
				if _, err := store.Get(ctx, ref); err == nil {
					t.Fatal("empty scope accepted")
				}
				if _, err := store.List(ctx, first.Scope, "", 0, 101); err == nil {
					t.Fatal("unbounded list accepted")
				}
			})
			t.Run("edit delete and optimistic version", func(t *testing.T) {
				db, other := queueDatabases(t, driver)
				store, peer := db.BotMessages(), other.BotMessages()
				ctx := context.Background()
				sent := sentMessage(1, 10)
				recordSent(t, store, sent)
				edited := sent.SentAt.Add(time.Minute)
				result, err := store.ConfirmEdited(ctx, sent.Reference, 1, edited)
				if err != nil || result != messages.Applied {
					t.Fatal("edit not recorded")
				}
				result, err = peer.ConfirmDeleted(ctx, sent.Reference, 1, edited.Add(time.Minute))
				if err != nil || result != messages.Conflict {
					t.Fatal("stale update applied")
				}
				row, err := peer.Get(ctx, sent.Reference)
				if err != nil || row.Version != 2 || row.EditedAt == nil || !row.EditedAt.Equal(edited) {
					t.Fatal("edit state not persisted")
				}
				result, err = peer.ConfirmDeleted(ctx, sent.Reference, 2, edited.Add(time.Minute))
				if err != nil || result != messages.Applied {
					t.Fatal("delete not recorded")
				}
				result, err = store.ConfirmDeleted(ctx, sent.Reference, 2, edited.Add(time.Minute))
				if err != nil || result != messages.Unchanged {
					t.Fatal("delete retry not idempotent")
				}
				result, err = store.ConfirmEdited(ctx, sent.Reference, 3, edited.Add(2*time.Minute))
				if err != nil || result != messages.Conflict {
					t.Fatal("deleted message edited")
				}
				result, err = store.RecordSent(ctx, sent)
				if err != nil || result != messages.Unchanged {
					t.Fatal("late send receipt failed")
				}
				row, err = store.Get(ctx, sent.Reference)
				if err != nil || row.DeletedAt == nil || row.Version != 3 {
					t.Fatal("deleted reference lost or resurrected")
				}
				page, err := store.List(ctx, sent.Scope, "", 0, 10)
				if err != nil || len(page) != 1 || page[0].DeletedAt == nil {
					t.Fatal("tombstone missing")
				}
			})
			t.Run("concurrent mutation", func(t *testing.T) {
				db, other := queueDatabases(t, driver)
				stores := []*BotMessages{db.BotMessages(), other.BotMessages()}
				sent := sentMessage(1, 10)
				recordSent(t, stores[0], sent)
				var wg sync.WaitGroup
				results := make(chan messages.Result, 2)
				for _, store := range stores {
					wg.Add(1)
					go func(store *BotMessages) {
						defer wg.Done()
						result, err := store.ConfirmEdited(context.Background(), sent.Reference, 1, sent.SentAt.Add(time.Minute))
						if err != nil {
							t.Error(err)
						}
						results <- result
					}(store)
				}
				wg.Wait()
				close(results)
				counts := map[messages.Result]int{}
				for result := range results {
					counts[result]++
				}
				if counts[messages.Applied] != 1 || counts[messages.Conflict] != 1 {
					t.Fatal("lost update not prevented")
				}
			})
			t.Run("upgrade preserves legacy data without guessing bot", func(t *testing.T) {
				db, _ := databasesAtVersion(t, driver, 2)
				ctx := context.Background()
				if err := db.orm.Exec("INSERT INTO queue_scopes (agent_id,chat_id) VALUES ('legacy',10)").Error; err != nil {
					t.Fatal(err)
				}
				if err := db.orm.Exec(`INSERT INTO queue_jobs (agent_id,chat_id,dedupe_key,kind,payload,status,max_attempts,available_at,created_at,updated_at) VALUES ('legacy',10,'keep','test','{}','pending',2,0,0,0)`).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.orm.Exec(`INSERT INTO bot_messages (agent_id,chat_id,message_id,delivery_key,part,sent_at) VALUES ('legacy',10,42,'keep',0,1700000000000)`).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				var count int64
				db.orm.Table("queue_jobs").Where("bot_id=0 AND dedupe_key='keep'").Count(&count)
				if count != 1 {
					t.Fatal("legacy queue data lost")
				}
				db.orm.Table("bot_messages").Where("bot_id=0 AND message_id=42").Count(&count)
				if count != 1 {
					t.Fatal("legacy reference lost")
				}
				if job, err := db.Queue().Claim(ctx, []string{"test"}, time.Minute); err != nil || job != nil {
					t.Fatal("unmapped legacy job must not execute")
				}
				registerTestBots(t, db)
				enqueue(t, db.Queue(), 1, "new", 2)
				if job := claim(t, db.Queue()); job.ID <= 1 || job.Scope.BotID != 1 {
					t.Fatal("sequence or bot mapping not preserved")
				}
				recordSent(t, db.BotMessages(), sentMessage(1, 10))
				if err := db.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := db.BotMessages().Get(ctx, sentMessage(1, 10).Reference); err != nil {
					t.Fatal("repeat migration lost reference")
				}
			})
		})
	}
}
