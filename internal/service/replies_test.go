package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
)

type revisionSender struct {
	testSender
	edits   map[int64]string
	deleted []int64
}

func (s *revisionSender) Edit(_ context.Context, _, id int64, text string) error {
	s.edits[id] = text
	return nil
}
func (s *revisionSender) Delete(_ context.Context, _, id int64) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func TestEditedReplyReusesMessages(t *testing.T) {
	e, m, _, scope := engineFixture(t)
	ctx := context.Background()
	sender := &revisionSender{edits: map[int64]string{}}
	e.Bots[1] = sender
	accept(t, e, scope, 1, "original", true)
	setReply := func(text string) {
		raw, _ := json.Marshal(map[string]any{"reply": text})
		m.reply = string(raw)
	}
	setReply(strings.Repeat("a", 3600))
	run := func() {
		t.Helper()
		job := take(t, e, "respond")
		if err := e.Respond(ctx, *job); err != nil {
			t.Fatal(err)
		}
		if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if sender.calls != 2 {
		t.Fatal("expected multipart initial reply")
	}
	edit := func(update int64, text string) {
		t.Helper()
		if err := e.DB.Agent().Accept(ctx, agent.Incoming{Scope: scope, MessageID: 1, UpdateID: update, UserID: 7, Text: text, Edited: true, Respond: true, SentAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	edit(2, "shorter")
	setReply("revised")
	run()
	if sender.calls != 2 || sender.edits[10001] != "revised" || len(sender.deleted) != 1 || sender.deleted[0] != 10002 {
		t.Fatalf("wrong revision: %+v", sender)
	}
	links, err := e.DB.Agent().ReplyLinks(ctx, scope, 1)
	if err != nil || len(links) != 1 || links[0].MessageID != 10001 {
		t.Fatal("active links", links, err)
	}
	edit(3, "longer")
	setReply(strings.Repeat("b", 3600))
	run()
	if sender.calls != 3 || len(sender.edits[10001]) != 3500 {
		t.Fatalf("wrong extension: %+v", sender)
	}
	links, err = e.DB.Agent().ReplyLinks(ctx, scope, 1)
	if err != nil || len(links) != 2 || links[1].MessageID != 10003 {
		t.Fatal("extended links", links, err)
	}
	if !strings.Contains(m.input[0].Content, "Tools are unavailable in this revision") {
		t.Fatal("missing edit execution policy")
	}
	other := scope
	other.BotID = 2
	links, err = e.DB.Agent().ReplyLinks(ctx, other, 1)
	if err != nil || len(links) != 0 {
		t.Fatal("cross-bot links", err)
	}
}
