package storage

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
)

func TestPoCStorage(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			db, peer := databasesAtVersion(t, driver, 26)
			ctx := context.Background()
			scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
			if err := db.Agent().Ready(ctx); err != nil {
				t.Fatal(err)
			}
			if err := db.Agent().BindEnvironment(ctx, true); err != nil {
				t.Fatal(err)
			}
			if err := peer.Agent().BindEnvironment(ctx, false); err == nil {
				t.Fatal("mock data accepted by live instance")
			}
			in := agent.Incoming{Scope: scope, UpdateID: 1, MessageID: 1, UserID: 7, Name: "test", Title: "test chat", ChatType: "private", Text: "first", SentAt: time.Now().Add(-2 * time.Hour), Respond: true}
			if err := db.Agent().Accept(ctx, in); err != nil {
				t.Fatal(err)
			}
			if err := peer.Agent().Accept(ctx, in); err != nil {
				t.Fatal(err)
			}
			var count int64
			db.orm.Table("queue_jobs").Count(&count)
			if count != 1 {
				t.Fatal("inbox delivery duplicated job")
			}
			in.UpdateID = 2
			in.MessageID = 2
			in.Text = "later"
			in.Respond = false
			if err := db.Agent().Accept(ctx, in); err != nil {
				t.Fatal(err)
			}
			c, _, err := db.Agent().ReplyContext(ctx, scope, 1)
			if err != nil || len(c.Messages) != 1 || c.Messages[0].Text != "first" {
				t.Fatal("reply sees future backlog")
			}
			wrong := scope
			wrong.BotID = 2
			c, err = db.Agent().Context(ctx, wrong, 0, 30)
			if err != nil || len(c.Messages) != 0 {
				t.Fatal("history mixed bots")
			}
			if _, err := db.Access().SetUserBlocked(ctx, "test", 7, true); err != nil {
				t.Fatal(err)
			}
			in.UpdateID = 3
			in.MessageID = 3
			in.Respond = true
			in.Text = "blocked"
			if err := db.Agent().Accept(ctx, in); err != nil {
				t.Fatal(err)
			}
			c, err = db.Agent().Context(ctx, scope, 0, 30)
			if err != nil || len(c.Messages) != 2 {
				t.Fatal("blocked traffic entered history")
			}
			if _, err := db.Access().SetUserBlocked(ctx, "test", 7, false); err != nil {
				t.Fatal(err)
			}
			bad := in
			bad.UpdateID = 4
			bad.Scope.BotID = 999
			if err := db.Agent().Accept(ctx, bad); err == nil {
				t.Fatal("invalid bot accepted")
			}
			db.orm.Table("telegram_inbox").Where("bot_id=999").Count(&count)
			if count != 0 {
				t.Fatal("inbox committed failed transaction")
			}
			task := agent.Task{ID: "due", AgentID: "test", BotID: 1, ChatID: 100, UserID: 7, Prompt: "reminder", RunAt: time.Now().Add(-time.Second).UnixMilli(), Active: true}
			if err := db.Agent().PutTask(ctx, task, 0); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for _, d := range []*Database{db, peer} {
				wg.Add(1)
				go func(d *Database) {
					defer wg.Done()
					if err := d.Agent().Tick(ctx, 24*time.Hour); err != nil {
						t.Error(err)
					}
				}(d)
			}
			wg.Wait()
			db.orm.Table("queue_jobs").Where("dedupe_key LIKE 'task:%'").Count(&count)
			if count != 1 {
				t.Fatal("scheduler replicas duplicated occurrence")
			}
			var stored agent.Task
			db.orm.Table("scheduled_tasks").Where("id='due'").Take(&stored)
			if stored.Active || stored.Version != 2 {
				t.Fatal("one-time task not advanced")
			}
			if err := db.Agent().PutTask(ctx, task, 1); err == nil {
				t.Fatal("stale task edit overwrote schedule")
			}
			if err := db.Agent().DeleteTask(ctx, "due", 2); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestEncryptedSettings(t *testing.T) {
	db, _ := databasesAtVersion(t, config.SQLite, 26)
	ctx := context.Background()
	settings, err := db.Settings(config.NewSecret(strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.Put(ctx, "llm.api_key", "never-log-me", 0); err != nil {
		t.Fatal(err)
	}
	var raw Setting
	db.orm.Table("runtime_settings").Take(&raw)
	if strings.Contains(raw.Value, "never-log-me") {
		t.Fatal("plaintext secret at rest")
	}
	values, rows, err := settings.Load(ctx)
	if err != nil || values["llm.api_key"] != "never-log-me" || rows[0].Value != "" {
		t.Fatal("secret redaction or decryption failed")
	}
	if err := settings.Put(ctx, "llm.api_key", "wrong", 0); err == nil {
		t.Fatal("version conflict ignored")
	}
	if err := settings.Put(ctx, "oidc.issuer", "https://bad.invalid", 0); err == nil {
		t.Fatal("OIDC override accepted")
	}
	wrong, _ := db.Settings(config.NewSecret(strings.Repeat("b", 40)))
	if _, _, err := wrong.Load(ctx); err == nil {
		t.Fatal("wrong key decrypted settings")
	}
	if err := settings.Delete(ctx, "llm.api_key", 1); err != nil {
		t.Fatal(err)
	}
}
func TestPollingLease(t *testing.T) {
	db, peer := databasesAtVersion(t, config.SQLite, 26)
	ctx := context.Background()
	offset, owned, err := db.Agent().PollLease(ctx, "test", 1, "owner-a")
	if err != nil || !owned || offset != 0 {
		t.Fatal("initial polling lease failed")
	}
	if _, owned, err := peer.Agent().PollLease(ctx, "test", 1, "owner-b"); err != nil || owned {
		t.Fatal("two polling owners")
	}
	if err := db.Agent().PollAdvance(ctx, "test", 1, "owner-a", 12); err != nil {
		t.Fatal(err)
	}
	if err := peer.Agent().PollAdvance(ctx, "test", 1, "owner-b", 13); err == nil {
		t.Fatal("wrong poll owner advanced offset")
	}
	db.orm.Exec("UPDATE bot_poll_leases SET lease_until=0")
	offset, owned, err = peer.Agent().PollLease(ctx, "test", 1, "owner-b")
	if err != nil || !owned || offset != 12 {
		t.Fatal("poll offset lost on recovery")
	}
}
