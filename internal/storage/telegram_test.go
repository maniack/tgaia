package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/messages"
	"github.com/maniack/tgaia/internal/queue"
)

func TestMessageReactionsPersistWithoutQueueingResponse(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			db, _ := databasesAtVersion(t, driver, 26)
			ctx := context.Background()
			scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
			if err := db.Agent().Accept(ctx, agent.Incoming{Scope: scope, UpdateID: 1, MessageID: 1, UserID: 7, Name: "Ada", Username: "ada", Title: "Team", ChatType: "supergroup", Text: "hello", SentAt: time.Now(), Respond: false}); err != nil {
				t.Fatal(err)
			}
			sentAt := time.Now().UTC()
			if _, err := db.BotMessages().RecordSent(ctx, messages.Sent{Reference: messages.Reference{Scope: scope, MessageID: 44}, DeliveryKey: "test", SentAt: sentAt}); err != nil {
				t.Fatal(err)
			}
			if err := db.orm.Exec("INSERT INTO conversation_messages (agent_id,bot_id,chat_id,thread_id,message_id,user_id,role,text,sent_at) VALUES (?,?,?,?,?,0,'assistant',?,?)", scope.AgentID, scope.BotID, scope.ChatID, 0, 44, "Thanks for the update", sentAt.UnixMilli()).Error; err != nil {
				t.Fatal(err)
			}
			update := agent.ReactionUpdate{AgentID: scope.AgentID, BotID: scope.BotID, ChatID: scope.ChatID, MessageID: 44, UpdateID: 10, ActorType: "user", ActorID: 8, ActorName: "Lin", ActorUsername: "lin", Reactions: []agent.Reaction{{Type: "emoji", Value: "👍", Count: 1}}, UpdatedAt: sentAt.Add(time.Minute)}
			if err := db.Agent().ApplyReaction(ctx, update); err != nil {
				t.Fatal(err)
			}
			var jobs int64
			db.orm.Table("queue_jobs").Count(&jobs)
			if jobs != 0 {
				t.Fatal("reaction queued a response")
			}
			context, err := db.Agent().Context(ctx, scope, 0, 20)
			if err != nil || len(context.Reactions) != 1 || context.Reactions[0].Value != "👍" || context.Reactions[0].ActorName != "Lin" || context.Reactions[0].MessageText != "Thanks for the update" {
				t.Fatalf("reaction missing from context: %+v err=%v", context.Reactions, err)
			}
			stale := update
			stale.UpdateID = 9
			stale.Reactions = []agent.Reaction{{Type: "emoji", Value: "👎", Count: 1}}
			if err := db.Agent().ApplyReaction(ctx, stale); err != nil {
				t.Fatal(err)
			}
			context, _ = db.Agent().Context(ctx, scope, 0, 20)
			if len(context.Reactions) != 1 || context.Reactions[0].Value != "👍" {
				t.Fatal("stale reaction replaced current state")
			}
			update.UpdateID = 11
			update.UpdatedAt = update.UpdatedAt.Add(time.Minute)
			update.Reactions = nil
			if err := db.Agent().ApplyReaction(ctx, update); err != nil {
				t.Fatal(err)
			}
			context, _ = db.Agent().Context(ctx, scope, 0, 20)
			if len(context.Reactions) != 0 {
				t.Fatal("removed reaction remained in context")
			}
			unknown := update
			unknown.MessageID = 999
			unknown.UpdateID = 12
			unknown.Reactions = []agent.Reaction{{Type: "emoji", Value: "🔥", Count: 1}}
			if err := db.Agent().ApplyReaction(ctx, unknown); err != nil {
				t.Fatal(err)
			}
			context, _ = db.Agent().Context(ctx, scope, 0, 20)
			if len(context.Reactions) != 0 {
				t.Fatal("reaction to an unknown bot message was stored")
			}
			anonymous := update
			anonymous.UpdateID = 13
			anonymous.ActorType = "anonymous"
			anonymous.ActorID = 0
			anonymous.ActorName = "Anonymous participants"
			anonymous.ActorUsername = ""
			anonymous.Reactions = []agent.Reaction{{Type: "custom_emoji", Value: "custom-1", Count: 4}, {Type: "paid", Count: 2}}
			if err := db.Agent().ApplyReaction(ctx, anonymous); err != nil {
				t.Fatal(err)
			}
			context, _ = db.Agent().Context(ctx, scope, 0, 20)
			if len(context.Reactions) != 2 || context.Reactions[0].ActorType != "anonymous" || context.Reactions[0].Count+context.Reactions[1].Count != 6 {
				t.Fatalf("anonymous reaction counts missing from context: %+v", context.Reactions)
			}
			update.UpdateID = 14
			update.Reactions = []agent.Reaction{{Type: "emoji", Value: "👍", Count: 1}}
			if err := db.Agent().ApplyReaction(ctx, update); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Access().SetUserBlocked(ctx, scope.AgentID, update.ActorID, true); err != nil {
				t.Fatal(err)
			}
			context, _ = db.Agent().Context(ctx, scope, 0, 20)
			for _, reaction := range context.Reactions {
				if reaction.ActorType == "user" && reaction.ActorID == update.ActorID {
					t.Fatal("blocked user's reaction remained in context")
				}
			}
		})
	}
}

func TestConversationBehaviorAndBotSenders(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			db, _ := databasesAtVersion(t, driver, 26)
			ctx := context.Background()
			first := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
			second := chat.Scope{AgentID: "test", BotID: 1, ChatID: 200}
			otherBot := chat.Scope{AgentID: "agent", BotID: 2, ChatID: 100}
			accept := func(scope chat.Scope, update, user int64, isBot bool, respond bool) {
				t.Helper()
				if err := db.Agent().Accept(ctx, agent.Incoming{Scope: scope, UpdateID: update, MessageID: update, UserID: user, Name: map[bool]string{true: "Grafana", false: "Ada"}[isBot], Username: map[bool]string{true: "grafana_bot", false: "ada"}[isBot], IsBot: isBot, ChatType: "supergroup", Title: "Team", Text: "status", SentAt: time.Now(), Respond: respond}); err != nil {
					t.Fatal(err)
				}
			}
			jobs := func(scope chat.Scope, actor int64) int64 {
				t.Helper()
				var count int64
				if err := scoped(db.orm.Table("queue_jobs"), scope).Where("kind='respond' AND actor_user_id=?", actor).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				return count
			}

			// Default behavior stores unaddressed group traffic without replying.
			accept(first, 1, 7, false, false)
			behavior, err := db.Agent().ChatBehavior(ctx, first)
			if err != nil || behavior.GroupResponse != "mentions" || behavior.RespondToBots || !behavior.LoopProtection || behavior.Version != 1 {
				t.Fatalf("unexpected default behavior: %+v err=%v", behavior, err)
			}
			behavior.GroupResponse = "all"
			if err := db.Agent().SetChatBehavior(ctx, behavior); err != nil {
				t.Fatal(err)
			}
			accept(first, 2, 7, false, false)
			accept(second, 3, 7, false, false)
			accept(otherBot, 4, 7, false, false)
			if jobs(first, 7) != 1 || jobs(second, 7) != 0 || jobs(otherBot, 7) != 0 {
				t.Fatal("all-messages behavior leaked across bot_id or chat_id")
			}

			// Bot messages are identified and retained, but do not answer until
			// the independent respond-to-bots option is enabled.
			accept(first, 5, 99, true, true)
			if jobs(first, 99) != 0 {
				t.Fatal("bot received a reply while respond_to_bots was disabled")
			}
			history, err := db.Agent().Context(ctx, first, 0, 20)
			if err != nil || len(history.Messages) == 0 || !history.Messages[len(history.Messages)-1].IsBot {
				t.Fatalf("bot identity missing from conversation context: %+v err=%v", history.Messages, err)
			}
			profile, err := db.Agent().TelegramProfile(ctx, first.AgentID, 99)
			if err != nil || !profile.IsBot {
				t.Fatalf("bot identity missing from profile: %+v err=%v", profile, err)
			}

			behavior, _ = db.Agent().ChatBehavior(ctx, first)
			behavior.RespondToBots = true
			behavior.LoopProtection = false
			if err := db.Agent().SetChatBehavior(ctx, behavior); err != nil {
				t.Fatal(err)
			}
			accept(first, 6, 99, true, false)
			if jobs(first, 99) != 1 {
				t.Fatal("enabled bot response did not enqueue")
			}

			behavior, _ = db.Agent().ChatBehavior(ctx, first)
			behavior.LoopProtection = true
			if err := db.Agent().SetChatBehavior(ctx, behavior); err != nil {
				t.Fatal(err)
			}
			for update := int64(7); update <= 12; update++ {
				accept(first, update, 99, true, false)
			}
			if jobs(first, 99) != 6 {
				t.Fatalf("loop protection did not stop after six recent replies: %d", jobs(first, 99))
			}
			behavior, _ = db.Agent().ChatBehavior(ctx, first)
			behavior.LoopProtection = false
			if err := db.Agent().SetChatBehavior(ctx, behavior); err != nil {
				t.Fatal(err)
			}
			accept(first, 13, 99, true, false)
			if jobs(first, 99) != 7 {
				t.Fatal("disabled loop protection still limited bot replies")
			}
			accept(first, 14, first.BotID, true, true)
			var own int64
			if err := scoped(db.orm.Table("conversation_messages"), first).Where("message_id=?", 14).Count(&own).Error; err != nil || own != 0 {
				t.Fatal("the receiving bot's own message was stored as incoming", err)
			}
		})
	}
}

func TestTopicMemoryEditsAndScheduling(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			db, _ := databasesAtVersion(t, driver, 26)
			ctx := context.Background()
			first := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100, ThreadID: 10}
			second := first
			second.ThreadID = 20
			in := agent.Incoming{Scope: first, UserID: 7, UpdateID: 1, MessageID: 1, Text: "original", SentAt: time.Now().Add(-2 * time.Hour), Respond: true}
			if err := db.Agent().Accept(ctx, in); err != nil {
				t.Fatal(err)
			}
			other := in
			other.Scope = second
			other.MessageID = 2
			other.UpdateID = 2
			other.Text = "other topic"
			other.Respond = false
			if err := db.Agent().Accept(ctx, other); err != nil {
				t.Fatal(err)
			}
			// Topics of one chat share history and memory; only delivery keeps the topic.
			c, err := db.Agent().Context(ctx, first, 0, 50)
			if err != nil || len(c.Messages) != 2 || c.Messages[0].Text != "original" || c.Messages[1].Text != "other topic" {
				t.Fatal("topic history not shared", err)
			}
			job, err := db.Queue().Claim(ctx, []string{"respond"}, time.Minute)
			if err != nil || job == nil || job.Scope.ThreadID != 10 {
				t.Fatal("queue lost topic", err)
			}
			if err := db.Agent().SaveSummary(ctx, *job, 0, c.Messages[1].ID, "old summary", 0); err != nil {
				t.Fatal(err)
			}
			if other, err := db.Agent().Context(ctx, second, 0, 50); err != nil || other.Summary.Content != "old summary" || len(other.Messages) != 0 {
				t.Fatal("summary not shared with the other topic", err)
			}
			var summaryTopic int64 = -1
			db.orm.Table("memory_summaries").Select("thread_id").Where("chat_id=100").Scan(&summaryTopic)
			if summaryTopic != 0 {
				t.Fatal("chat-wide summary must live in topic 0")
			}
			in.Edited = true
			in.UpdateID = 3
			in.Text = "corrected"
			if err := db.Agent().Accept(ctx, in); err != nil {
				t.Fatal(err)
			}
			c, err = db.Agent().Context(ctx, first, 0, 50)
			if err != nil || c.Summary.Content != "" || len(c.Messages) != 2 || c.Messages[0].Text != "corrected" {
				t.Fatal("edit not reflected", err)
			}
			if err := db.Agent().SaveSummary(ctx, *job, 0, c.Messages[1].ID, "stale", 0); err == nil {
				t.Fatal("stale compaction committed")
			}
			if current, err := db.Agent().ResponseCurrent(ctx, first, 1, 1); err != nil || current {
				t.Fatal("stale response allowed")
			}
			in.UpdateID = 0
			in.Text = "late old edit"
			if err := db.Agent().Accept(ctx, in); err != nil {
				t.Fatal(err)
			}
			c, _ = db.Agent().Context(ctx, first, 0, 50)
			if c.Messages[0].Text != "corrected" {
				t.Fatal("out of order edit replaced latest")
			}
			if err := db.Queue().Complete(ctx, job.Receipt); err != nil {
				t.Fatal(err)
			}
			task := agent.Task{ID: "topic-task", AgentID: "test", BotID: 1, ChatID: 100, ThreadID: 20, UserID: 7, Prompt: "topic reminder", RunAt: time.Now().Add(-time.Second).UnixMilli(), Active: true}
			if err := db.Agent().PutTask(ctx, task, 0); err != nil {
				t.Fatal(err)
			}
			if err := db.Agent().Tick(ctx, 24*time.Hour); err != nil {
				t.Fatal(err)
			}
			var rows []struct {
				ThreadID int64
				Payload  string
			}
			db.orm.Table("queue_jobs").Where("dedupe_key LIKE 'task:%'").Find(&rows)
			if len(rows) != 1 || rows[0].ThreadID != 20 {
				t.Fatal("scheduler lost topic")
			}
			var p agent.JobPayload
			if json.Unmarshal([]byte(rows[0].Payload), &p) != nil || p.Prompt != "topic reminder" {
				t.Fatal("task payload")
			}
			// Queue serialization remains chat-wide; history and tasks are topic-scoped.
			_, err = db.Queue().Enqueue(ctx, queue.Input{Scope: second, Key: "service", Kind: "test", Payload: []byte(`{}`), MaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
