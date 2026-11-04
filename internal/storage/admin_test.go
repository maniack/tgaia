package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
)

func TestAdminPaginationAndResolution(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			db, peer := databasesAtVersion(t, driver, 17)
			ctx := context.Background()
			sc := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
			for i := int64(1); i <= 125; i++ {
				if err := db.Agent().Accept(ctx, agent.Incoming{Scope: sc, UpdateID: i, MessageID: i, UserID: i, Text: fmt.Sprint(i), SentAt: time.Now(), Respond: i == 1}); err != nil {
					t.Fatal(err)
				}
			}
			for _, kind := range []string{"history", "users"} {
				cursor := ""
				seen := map[string]bool{}
				n := 0
				for {
					p, err := db.Agent().AdminPage(ctx, kind, cursor, 17, sc)
					if err != nil {
						t.Fatal(err)
					}
					for _, row := range p.Items {
						key := fmt.Sprint(row["user_id"])
						if seen[key] {
							t.Fatal("duplicate cursor row")
						}
						seen[key] = true
						n++
					}
					if p.Next == "" {
						break
					}
					cursor = p.Next
				}
				if n != 125 {
					t.Fatalf("%s got %d", kind, n)
				}
			}
			p, err := db.Agent().AdminPage(ctx, "history", "", 10, sc)
			if err != nil {
				t.Fatal(err)
			}
			other := sc
			other.ChatID = 101
			if _, err = db.Agent().AdminPage(ctx, "history", p.Next, 10, other); err == nil {
				t.Fatal("cross-scope cursor accepted")
			}
			for _, kind := range []string{"bots", "chats", "tasks", "deliveries", "tools"} {
				if _, err = db.Agent().AdminPage(ctx, kind, "", 10, chat.Scope{}); err != nil {
					t.Fatal(kind, err)
				}
			}
			job, err := db.Queue().Claim(ctx, []string{"respond"}, time.Minute)
			if err != nil || job == nil {
				t.Fatal(err)
			}
			if _, err = db.Agent().BeginSend(ctx, *job, 0, "reply"); err != nil {
				t.Fatal(err)
			}
			if err = db.Agent().MarkUncertain(ctx, job.ID, 0); err != nil {
				t.Fatal(err)
			}
			resolution := Resolution{JobID: job.ID, Target: "delivery:0", Version: 2, Action: "confirm", MessageID: 999, SentAt: time.Now().UnixMilli(), Reason: "Verified Telegram receipt"}
			if err = peer.Agent().ResolveOutcome(ctx, resolution, "admin"); !errors.Is(err, ErrResolutionConflict) {
				t.Fatal("active worker not protected", err)
			}
			if err = db.Queue().Complete(ctx, job.Receipt); err != nil {
				t.Fatal(err)
			}
			bad := resolution
			bad.Version = 1
			if err = peer.Agent().ResolveOutcome(ctx, bad, "admin"); !errors.Is(err, ErrResolutionConflict) {
				t.Fatal("stale version accepted", err)
			}
			if err = peer.Agent().ResolveOutcome(ctx, resolution, "admin"); err != nil {
				t.Fatal(err)
			}
			if err = peer.Agent().ResolveOutcome(ctx, resolution, "admin"); !errors.Is(err, ErrResolutionConflict) {
				t.Fatal("double resolution", err)
			}
			if err = db.Agent().Sent(ctx, *job, 0, "stale worker", 998, time.Now()); err == nil {
				t.Fatal("stale worker wrote receipt")
			}
			history, err := db.Agent().AdminPage(ctx, "history", "", 1, sc)
			if err != nil || fmt.Sprint(history.Items[0]["message_id"]) != "999" {
				t.Fatal("receipt missing", err)
			}
			var count int64
			db.orm.Table("outcome_resolutions").Where("subject='admin'").Count(&count)
			if count != 1 {
				t.Fatal("audit missing")
			}
			job, err = db.Queue().Claim(ctx, []string{"respond"}, time.Minute)
			if err != nil || job == nil {
				t.Fatal("remaining response not resumed", err)
			}
			if fresh, err := db.Agent().BeginSend(ctx, *job, 0, "reply"); err != nil || fresh {
				t.Fatal("resolved delivery can resend", err)
			}
			if _, err = db.Agent().StartTool(ctx, *job, "call:0:0", `{"name":"test"}`); err != nil {
				t.Fatal(err)
			}
			if err = db.Queue().Complete(ctx, job.Receipt); err != nil {
				t.Fatal(err)
			}
			if err = peer.Agent().ResolveOutcome(ctx, Resolution{JobID: job.ID, Target: "call:0:0", Version: 1, Action: "dismiss", Reason: "No reliable external receipt"}, "admin"); err != nil {
				t.Fatal(err)
			}
			step, err := db.Agent().ToolStep(ctx, job.ID, "call:0:0")
			if err != nil || step.State != "uncertain" {
				t.Fatal("dismissed tool can resume external effects", err)
			}
		})
	}
}
