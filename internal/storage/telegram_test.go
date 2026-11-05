package storage

import (
	"context"
	"encoding/json"
	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/queue"
	"testing"
	"time"
)

func TestTopicMemoryEditsAndScheduling(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			db, _ := databasesAtVersion(t, driver, 20)
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
