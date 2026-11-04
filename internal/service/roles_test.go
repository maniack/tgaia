package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/access"
	"github.com/maniack/tgaia/internal/agent"
)

func TestReaderCannotActAndPromotionIsLive(t *testing.T) {
	e, m, s, sc := engineFixture(t)
	ctx := context.Background()
	accept(t, e, sc, 1, "register", false)
	if err := e.DB.Agent().SetRole(ctx, sc.AgentID, 7, "reader", 1); err != nil {
		t.Fatal(err)
	}
	f := &toolFixture{}
	e.Tools = f
	m.reply = `{"reply":"read-only answer"}`
	accept(t, e, sc, 2, "do something", true)
	j := take(t, e, "respond")
	if err := e.Respond(ctx, *j); err != nil {
		t.Fatal(err)
	}
	if s.calls != 1 || f.calls != 0 || f.plans != 0 {
		t.Fatal("reader tools/reply", s.calls, f)
	}
	if err := e.DB.Queue().Complete(ctx, j.Receipt); err != nil {
		t.Fatal(err)
	}
	d, err := e.DB.Agent().Dashboard(ctx)
	if err != nil || len(d.Tasks) != 0 {
		t.Fatal("reader created task", err)
	}
	memory, err := e.DB.Agent().Context(ctx, sc, 0, 30)
	if err != nil || len(memory.ExplicitMemories) != 0 {
		t.Fatal("reader created explicit memory", err)
	}
	task := agent.Task{ID: "test", AgentID: sc.AgentID, BotID: sc.BotID, ChatID: sc.ChatID, UserID: 7, Prompt: "test", RunAt: time.Now().Add(time.Hour).UnixMilli(), Active: true}
	if err := e.DB.Agent().PutTask(ctx, task, 0); !errors.Is(err, access.ErrDenied) {
		t.Fatal("storage accepted reader task", err)
	}
	if err := e.DB.Agent().SetRole(ctx, sc.AgentID, 7, "user", 1); err == nil {
		t.Fatal("stale role write accepted")
	}
	if err := e.DB.Agent().SetRole(ctx, sc.AgentID, 7, "user", 2); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Agent().PutTask(ctx, task, 0); err != nil {
		t.Fatal(err)
	}
	e.Model = f
	accept(t, e, sc, 3, "lookup", true)
	j = take(t, e, "respond")
	if err := e.Respond(ctx, *j); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatal("promoted user tools unavailable")
	}
}
func TestRevocationRetiresWorkAndUnblockDoesNotReplay(t *testing.T) {
	for _, mode := range []string{"role", "user", "chat"} {
		t.Run(mode, func(t *testing.T) {
			e, _, _, sc := engineFixture(t)
			ctx := context.Background()
			accept(t, e, sc, 1, "pending", true)
			j := take(t, e, "respond")
			task := agent.Task{ID: "test", AgentID: sc.AgentID, BotID: sc.BotID, ChatID: sc.ChatID, UserID: 7, Prompt: "test", RunAt: time.Now().Add(time.Hour).UnixMilli(), Active: true}
			if err := e.DB.Agent().PutTask(ctx, task, 0); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "role":
				if err := e.DB.Agent().SetRole(ctx, sc.AgentID, 7, "reader", 1); err != nil {
					t.Fatal(err)
				}
				if err := e.DB.Agent().SetRole(ctx, sc.AgentID, 7, "user", 2); err != nil {
					t.Fatal(err)
				}
			case "user":
				if _, err := e.DB.Access().SetUserBlocked(ctx, sc.AgentID, 7, true); err != nil {
					t.Fatal(err)
				}
				if _, err := e.DB.Access().SetUserBlocked(ctx, sc.AgentID, 7, false); err != nil {
					t.Fatal(err)
				}
			case "chat":
				if err := e.DB.Agent().BlockChat(ctx, sc, true); err != nil {
					t.Fatal(err)
				}
				if err := e.DB.Agent().BlockChat(ctx, sc, false); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.DB.Queue().Complete(ctx, j.Receipt); err == nil {
				t.Fatal("revoked lease survived")
			}
			if next, err := e.DB.Queue().Claim(ctx, []string{"respond"}, time.Minute); err != nil || next != nil {
				t.Fatal("suppressed work replayed", err)
			}
			d, err := e.DB.Agent().Dashboard(ctx)
			if err != nil || len(d.Tasks) != 1 || d.Tasks[0].Active {
				t.Fatal("task resumed", err)
			}
			accept(t, e, sc, 2, "new after restore", true)
			take(t, e, "respond")
		})
	}
}
