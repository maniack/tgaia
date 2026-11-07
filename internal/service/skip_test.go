package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
)

func groupMessage(t *testing.T, e *Engine, scope chat.Scope, id int64, respond bool) {
	t.Helper()
	if err := e.DB.Agent().Accept(context.Background(), agent.Incoming{Scope: scope, UpdateID: id, MessageID: id, UserID: 7, ChatType: "supergroup", Title: "group", Text: "сообщение", SentAt: time.Now(), Respond: respond}); err != nil {
		t.Fatal(err)
	}
}

func allowSkip(t *testing.T, e *Engine, scope chat.Scope) {
	t.Helper()
	groupMessage(t, e, scope, 1, false) // Creates the chat directory row.
	b, err := e.DB.Agent().ChatBehavior(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	b.GroupResponse, b.AllowSkip = "all", true
	if err := e.DB.Agent().SetChatBehavior(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

// An overheard group message may be skipped; the model sees the skip schema.
func TestSkipAllowedOnlyForUnaddressedMessages(t *testing.T) {
	e, _, sender, scope := engineFixture(t)
	m := &scriptedModel{reply: `{"reply":"","skip":true}`}
	e.Model = m
	allowSkip(t, e, scope)
	groupMessage(t, e, scope, 2, false)
	job := take(t, e, "respond")
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(context.Background(), job.Receipt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.schema, "skip") || sender.calls != 0 {
		t.Fatalf("unaddressed: schema=%s sends=%d", m.schema, sender.calls)
	}

	// A mention gets the plain schema, and a skip answer is rejected.
	groupMessage(t, e, scope, 3, true)
	err := e.Respond(context.Background(), *take(t, e, "respond"))
	if strings.Contains(m.schema, "skip") || err == nil || sender.calls != 0 {
		t.Fatalf("addressed: schema=%s err=%v sends=%d", m.schema, err, sender.calls)
	}
}
