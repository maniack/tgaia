package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/queue"
	"github.com/maniack/tgaia/internal/storage"
)

type testModel struct {
	calls int
	input []llm.Message
	reply string
}

func (m *testModel) Complete(ctx context.Context, in []llm.Message) (string, error) {
	m.calls++
	m.input = in
	return m.reply, nil
}

type testSender struct {
	calls int
	fail  bool
}

func (s *testSender) Send(ctx context.Context, chatID int64, text string) (int64, time.Time, error) {
	s.calls++
	if s.fail {
		return 0, time.Time{}, errors.New("unknown external result")
	}
	return int64(10000 + s.calls), time.Now().UTC(), nil
}
func (*testSender) Edit(context.Context, int64, int64, string) error { return nil }
func (*testSender) Delete(context.Context, int64, int64) error       { return nil }
func engineFixture(t *testing.T) (*Engine, *testModel, *testSender, chat.Scope) {
	t.Helper()
	db, err := storage.Open(context.Background(), config.Config{Database: config.Database{Driver: config.SQLite, DSN: config.NewSecret(filepath.Join(t.TempDir(), "poc.db"))}, ConnectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if _, err := db.Bots().Register(context.Background(), "a", id, "TOKEN"); err != nil {
			t.Fatal(err)
		}
	}
	promptFile := filepath.Join(t.TempDir(), "system-prompt.md")
	if err := os.WriteFile(promptFile, []byte("You are a personal assistant."), 0600); err != nil {
		t.Fatal(err)
	}
	model := &testModel{reply: `{"reply":"test answer","schedule":null}`}
	sender := &testSender{}
	return &Engine{DefaultPromptFile: promptFile, DB: db, Model: model, Bots: map[int64]Sender{1: sender, 2: sender}}, model, sender, chat.Scope{AgentID: "a", BotID: 1, ChatID: 100}
}
func accept(t *testing.T, e *Engine, scope chat.Scope, id int64, text string, respond bool) {
	t.Helper()
	if err := e.DB.Agent().Accept(context.Background(), agent.Incoming{Scope: scope, UpdateID: id, MessageID: id, UserID: 7, ChatType: "private", Title: "test", Text: text, SentAt: time.Now().Add(-2 * time.Hour), Respond: respond}); err != nil {
		t.Fatal(err)
	}
}
func take(t *testing.T, e *Engine, kind string) *queue.Job {
	t.Helper()
	job, err := e.DB.Queue().Claim(context.Background(), []string{kind}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim %s: %v", kind, err)
	}
	return job
}
func TestResponsePersistenceAndIsolation(t *testing.T) {
	e, model, sender, scope := engineFixture(t)
	ctx := context.Background()
	if err := e.DB.Bots().SetPrompt(ctx, "a", 1, "Unique bot instructions"); err != nil {
		t.Fatal(err)
	}
	accept(t, e, scope, 1, "target", true)
	accept(t, e, scope, 2, "future message must not enter target reply", false)
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.input[0].Content, "Unique bot instructions") || strings.Contains(model.input[1].Content, "future message") {
		t.Fatal("wrong prompt/context")
	}
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 || model.calls != 1 {
		t.Fatal("retry repeated saved model result or sent message")
	}
	refs, err := e.DB.BotMessages().List(ctx, scope, "", 0, 10)
	if err != nil || len(refs) != 1 {
		t.Fatal("message ID not stored")
	}
	other := scope
	other.BotID = 2
	c, err := e.DB.Agent().Context(ctx, other, 0, 30)
	if err != nil || len(c.Messages) != 0 || c.Summary.Content != "" {
		t.Fatal("another bot saw memory")
	}
}
func TestUncertainSendNotDuplicated(t *testing.T) {
	e, _, sender, scope := engineFixture(t)
	sender.fail = true
	accept(t, e, scope, 1, "hello", true)
	job := take(t, e, "respond")
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	sender.fail = false
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 {
		t.Fatal("unknown send duplicated")
	}
	state, err := e.DB.Agent().DeliveryState(context.Background(), job.ID, 0)
	if err != nil || state != "uncertain" {
		t.Fatal("uncertain state lost")
	}
}
func TestCompactionContinuesAfterInputLimit(t *testing.T) {
	e, model, _, scope := engineFixture(t)
	model.reply = "compact memory"
	ctx := context.Background()
	for i := int64(1); i <= 100; i++ {
		accept(t, e, scope, i, strings.Repeat("x", 2000), false)
	}
	if err := e.DB.Agent().Tick(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	job := take(t, e, "compact")
	if err := e.Compact(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}
	c, err := e.DB.Agent().Context(ctx, scope, 0, 200)
	if err != nil || c.Summary.ThroughID == 0 || len(c.Messages) == 0 {
		t.Fatal("compaction skipped messages outside actual input")
	}
	old := c.Summary.ThroughID
	if err := e.DB.Agent().Tick(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	job = take(t, e, "compact")
	if err := e.Compact(ctx, *job); err != nil {
		t.Fatal(err)
	}
	c, err = e.DB.Agent().Context(ctx, scope, 0, 200)
	if err != nil || c.Summary.ThroughID <= old {
		t.Fatal("partial compaction cannot continue")
	}
}
func TestClosedModelSchema(t *testing.T) {
	for _, text := range []string{`{"reply":"ok","scope":"other"}`, `{"reply":"ok","schedule":{"prompt":"x","delay_seconds":1,"interval_seconds":0}}`, `{"reply":"ok"} {"reply":"again"}`} {
		if _, err := decodeResponse(text); err == nil {
			t.Fatal("invalid model action accepted")
		}
	}
}

func TestEditedMemoryCanBeCompactedAgain(t *testing.T) {
	e, model, _, scope := engineFixture(t)
	ctx := context.Background()
	accept(t, e, scope, 1, "hello", true)
	response := take(t, e, "respond")
	if err := e.Respond(ctx, *response); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, response.Receipt); err != nil {
		t.Fatal(err)
	}
	model.reply = "old summary"
	time.Sleep(2 * time.Millisecond)
	if err := e.DB.Agent().Tick(ctx, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	first := take(t, e, "compact")
	if err := e.Compact(ctx, *first); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, first.Receipt); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Agent().SyncMessage(ctx, scope, 10001, "corrected answer", false); err != nil {
		t.Fatal(err)
	}
	c, err := e.DB.Agent().Context(ctx, scope, 0, 200)
	if err != nil || c.Summary.Content != "" {
		t.Fatal("old summary survived edit")
	}
	if err := e.DB.Agent().Tick(ctx, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	next := take(t, e, "compact")
	if next.ID == first.ID {
		t.Fatal("reused completed compaction")
	}
	model.reply = "new summary"
	if err := e.Compact(ctx, *next); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.input[1].Content, "corrected answer") {
		t.Fatal("compaction lost correction")
	}
}
