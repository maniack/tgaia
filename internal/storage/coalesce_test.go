package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
)

func TestCoalescingAcrossSendersTopicsAndAddressing(t *testing.T) {
	old := CoalescingDelay
	CoalescingDelay = time.Minute
	defer func() { CoalescingDelay = old }()
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			db, _ := databasesAtVersion(t, driver, 28)
			ctx := context.Background()
			scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
			msg := func(id, user int64, thread int64, bot, respond bool) {
				t.Helper()
				s := scope
				s.ThreadID = thread
				if err := db.Agent().Accept(ctx, agent.Incoming{Scope: s, UpdateID: id, MessageID: id, UserID: user, IsBot: bot, Title: "Team", ChatType: "supergroup", IsForum: true, Text: "m", SentAt: time.Now(), Respond: respond}); err != nil {
					t.Fatal(err)
				}
			}
			msg(1, 7, 0, false, false) // Creates the chat row.
			b, err := db.Agent().ChatBehavior(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			b.GroupResponse, b.RespondToBots, b.LoopProtection = "all", true, true
			if err := db.Agent().SetChatBehavior(ctx, b); err != nil {
				t.Fatal(err)
			}
			msg(2, 7, 0, false, true)  // Mention: addressed.
			msg(3, 7, 0, false, false) // Overheard follow-up folds into job of 2.
			msg(4, 7, 5, false, false) // Another topic: its own job.
			msg(5, 9, 0, true, false)  // Bot text...
			msg(6, 9, 0, true, false)  // ...and its attachment fold together.
			var jobs []struct {
				ThreadID    int64
				ActorUserID int64
				Payload     string
			}
			if err := db.orm.Raw("SELECT thread_id,actor_user_id,payload FROM queue_jobs WHERE kind='respond' ORDER BY id").Scan(&jobs).Error; err != nil {
				t.Fatal(err)
			}
			// Job of message 1 was created before group_response=all, so it is not there.
			if len(jobs) != 3 {
				t.Fatalf("jobs = %+v", jobs)
			}
			if !strings.Contains(jobs[0].Payload, `"message_id":3`) || !strings.Contains(jobs[0].Payload, `"addressed":true`) {
				t.Fatalf("merged human job = %s", jobs[0].Payload)
			}
			if jobs[1].ThreadID != 5 || strings.Contains(jobs[1].Payload, "addressed") {
				t.Fatalf("topic job = %+v", jobs[1])
			}
			if jobs[2].ActorUserID != 9 || !strings.Contains(jobs[2].Payload, `"message_id":6`) {
				t.Fatalf("bot job = %+v", jobs[2])
			}
		})
	}
}
