package service

import (
	"context"
	"encoding/json"
	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/llm"
	"strings"
	"testing"
	"time"
)

type visionFixture struct {
	input []llm.Message
	calls int
}

func (m *visionFixture) Complete(_ context.Context, in []llm.Message) (string, error) {
	m.calls++
	if len(in) > 1 && len(in[1].Images) > 0 {
		return "A red square", nil
	}
	m.input = in
	return `{"reply":"A red square"}`, nil
}

type mediaSender struct {
	testSender
	fetches int
	topic   int64
}

func (s *mediaSender) Fetch(context.Context, agent.Attachment) ([]byte, string, error) {
	s.fetches++
	return []byte("fixture-image"), "image/png", nil
}
func (s *mediaSender) SendTopic(ctx context.Context, chat, topic int64, text string) (int64, time.Time, error) {
	s.topic = topic
	return s.Send(ctx, chat, text)
}
func TestImageContextIdentityAndUTC(t *testing.T) {
	e, _, _, sc := engineFixture(t)
	sc.ThreadID = 42
	ctx := context.Background()
	m := &visionFixture{}
	sender := &mediaSender{}
	e.Model = m
	e.Bots[1] = sender
	e.Identities = map[int64]BotIdentity{1: {Name: "Test assistant", Username: "test_bot"}}
	e.Now = func() time.Time { return time.Date(2026, 11, 4, 3, 0, 0, 0, time.FixedZone("test", 7200)) }
	in := agent.Incoming{Scope: sc, MessageID: 1, UpdateID: 1, UserID: 7, Text: "what is here", Respond: true, SentAt: time.Now(), Attachments: []agent.Attachment{{Kind: "image", FileID: "file"}}}
	if err := e.DB.Agent().Accept(ctx, in); err != nil {
		t.Fatal(err)
	}
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if sender.topic != 42 || sender.fetches != 1 || m.calls != 2 {
		t.Fatal("image/topic pipeline", sender, m.calls)
	}
	if !strings.Contains(m.input[1].Content, "A red square") || !strings.Contains(m.input[0].Content, "2026-11-04T01:00:00Z") || !strings.Contains(m.input[0].Content, "Test assistant") || !strings.Contains(m.input[0].Content, "test_bot") {
		t.Fatal("missing runtime/media context")
	}
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if sender.fetches != 1 || sender.calls != 1 {
		t.Fatal("retry repeated media/send")
	}
	c, err := e.DB.Agent().Context(ctx, sc, 0, 50)
	if err != nil || c.Messages[0].MediaText != "A red square\n" {
		t.Fatal("description not persisted", err)
	}
}
func TestAnonymousCannotSchedule(t *testing.T) {
	e, m, _, sc := engineFixture(t)
	ctx := context.Background()
	m.reply = `{"reply":"text"}`
	if err := e.DB.Agent().Accept(ctx, agent.Incoming{Scope: sc, MessageID: 1, UpdateID: 1, SenderChatID: -100, Text: "@bot remind", Respond: true, SentAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	job := take(t, e, "respond")
	if job.SenderChatID != -100 || job.ActorUserID != 0 {
		t.Fatal("hidden principal lost")
	}
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	d, err := e.DB.Agent().Dashboard(ctx)
	if err != nil || len(d.Tasks) != 0 {
		t.Fatal("anonymous scheduled task", err)
	}
	raw, _ := json.Marshal(m.input)
	if !strings.Contains(string(raw), "reader-only access") {
		t.Fatal("reader instructions missing")
	}
}
