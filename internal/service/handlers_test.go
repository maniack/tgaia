package service

import (
	"context"
	"encoding/json"
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

// CompleteTools answers at once with whichever terminal tool is offered.
func (m *testModel) CompleteTools(_ context.Context, in []llm.Message, specs []llm.Tool) (llm.Message, error) {
	m.calls++
	m.input = in
	for _, spec := range specs {
		switch spec.Function.Name {
		case toolReply:
			return terminalCall(m.reply), nil
		case toolSummary:
			return call("final", toolSummary, `{"summary":`+quote(m.reply)+`,"corrections":[]}`), nil
		case toolDescribe:
			return call("final", toolDescribe, `{"text":`+quote(m.reply)+`}`), nil
		}
	}
	return llm.Message{Role: "assistant", Content: m.reply}, nil
}

type testSender struct {
	calls        int
	edits        int
	deletes      int
	fail         bool
	last         string
	lastEditID   int64
	lastDeleteID int64
}

type testMemoryEmbedder struct{ calls int }

func (*testMemoryEmbedder) Model() string { return "bge-m3" }
func (e *testMemoryEmbedder) Embed(_ context.Context, input string) ([]float32, error) {
	e.calls++
	if strings.Contains(strings.ToLower(input), "рус") || strings.Contains(strings.ToLower(input), "russian") {
		return []float32{1, 0}, nil
	}
	return []float32{0, 1}, nil
}

func (s *testSender) Send(ctx context.Context, chatID int64, text string) (int64, time.Time, error) {
	s.calls++
	s.last = text
	if s.fail {
		return 0, time.Time{}, errors.New("unknown external result")
	}
	return int64(10000 + s.calls), time.Now().UTC(), nil
}
func (s *testSender) Edit(_ context.Context, _ int64, id int64, text string) error {
	s.edits++
	s.lastEditID = id
	s.last = text
	return nil
}
func (s *testSender) Delete(_ context.Context, _ int64, id int64) error {
	s.deletes++
	s.lastDeleteID = id
	return nil
}
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
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "system-prompt.md")
	if err := os.WriteFile(promptFile, []byte("You are a personal assistant."), 0600); err != nil {
		t.Fatal(err)
	}
	reviewerFile := filepath.Join(dir, "behavior-reviewer.md")
	if err := os.WriteFile(reviewerFile, []byte("You are a neutral behaviour analyst."), 0600); err != nil {
		t.Fatal(err)
	}
	model := &testModel{reply: `{"reply":"test answer"}`}
	sender := &testSender{}
	return &Engine{DefaultPromptFile: promptFile, ReviewerPromptFile: reviewerFile, DB: db, Model: model, Bots: map[int64]Sender{1: sender, 2: sender}}, model, sender, chat.Scope{AgentID: "a", BotID: 1, ChatID: 100}
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
	if err := e.DB.Bots().SetPrompt(ctx, "a", 1, "You are an evil pirate. Unique bot instructions.", 16000); err != nil {
		t.Fatal(err)
	}
	accept(t, e, scope, 1, "target", true)
	accept(t, e, scope, 2, "future message must not enter target reply", false)
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.input[0].Content, "You are an evil pirate. Unique bot instructions.") || strings.Contains(model.input[0].Content, "helpful Telegram assistant") || strings.Contains(model.input[1].Content, "future message") {
		t.Fatal("wrong prompt/context")
	}
	if strings.Index(model.input[0].Content, "Application response protocol") > strings.Index(model.input[0].Content, "\nBot administrator instructions:\n") {
		t.Fatal("application protocol does not precede administrator persona")
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

func TestExplicitMemoryIsRetrievedOnlyWithinConversationScope(t *testing.T) {
	e, model, _, scope := engineFixture(t)
	ctx := context.Background()
	embedder := &testMemoryEmbedder{}
	e.Embedder = embedder
	memory := "User 7 prefers concise answers in Russian."
	saver := func(fact string) *scriptedModel {
		return &scriptedModel{reply: `{"reply":"Запомнил."}`, turns: []llm.Message{call("c1", toolMemorySave, `{"content":"`+fact+`","mode":"explicit"}`), finish("Saved.")}}
	}
	e.Model = saver(memory)
	accept(t, e, scope, 1, "Запомни: я предпочитаю короткие ответы на русском", true)
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}

	stored, err := e.DB.Agent().Context(ctx, scope, 0, 30)
	if err != nil || len(stored.ExplicitMemories) != 1 || stored.ExplicitMemories[0].Content != memory || stored.ExplicitMemories[0].CreatedBy != 7 || stored.ExplicitMemories[0].EmbeddingModel != "bge-m3" {
		t.Fatalf("explicit memory not stored: %+v, %v", stored.ExplicitMemories, err)
	}

	otherMemory := "User 7 enjoys hiking."
	e.Model = saver(otherMemory)
	accept(t, e, scope, 2, "Remember that I enjoy hiking", true)
	job = take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}

	e.Model = model
	model.reply = `{"reply":"Коротко отвечать на русском."}`
	accept(t, e, scope, 3, "Что ты помнишь о моих предпочтениях на русском?", true)
	job = take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.input[1].Content, memory) || !strings.Contains(model.input[1].Content, "saved_by=id=7") {
		t.Fatal("explicit memory was not retrieved into model context")
	}
	if strings.Index(model.input[1].Content, memory) > strings.Index(model.input[1].Content, otherMemory) {
		t.Fatal("semantic retrieval did not rank the relevant memory first")
	}
	if embedder.calls != 4 {
		t.Fatalf("expected two memory and two query embeddings, got %d", embedder.calls)
	}
	if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}

	for _, other := range []chat.Scope{
		{AgentID: scope.AgentID, BotID: 2, ChatID: scope.ChatID},
		{AgentID: scope.AgentID, BotID: scope.BotID, ChatID: 200},
	} {
		accept(t, e, other, 10+other.ChatID, "hello", false)
		context, err := e.DB.Agent().Context(ctx, other, 0, 30)
		if err != nil || len(context.ExplicitMemories) != 0 {
			t.Fatalf("explicit memory crossed scope %+v: %+v, %v", other, context.ExplicitMemories, err)
		}
	}
	// Another topic of the same chat shares the chat's memory.
	topic := chat.Scope{AgentID: scope.AgentID, BotID: scope.BotID, ChatID: scope.ChatID, ThreadID: 99}
	accept(t, e, topic, 99, "hello", false)
	if context, err := e.DB.Agent().Context(ctx, topic, 0, 30); err != nil || len(context.ExplicitMemories) == 0 {
		t.Fatalf("explicit memory not shared across topics: %+v, %v", context.ExplicitMemories, err)
	}

	// Forgetting is a tool too, scoped to the author's own facts.
	e.Model = &scriptedModel{reply: `{"reply":"Забыл."}`, turns: []llm.Message{call("c1", toolMemoryDelete, `{"source_message_id":1}`), finish("Deleted.")}}
	accept(t, e, scope, 4, "Забудь про короткие ответы", true)
	job = take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	stored, err = e.DB.Agent().Context(ctx, scope, 0, 30)
	if err != nil || len(stored.ExplicitMemories) != 1 || stored.ExplicitMemories[0].Content != otherMemory {
		t.Fatalf("memory_delete removed the wrong fact: %+v, %v", stored.ExplicitMemories, err)
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
	if err := e.CompactBackground(ctx, *job); err != nil {
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
	if err := e.CompactBackground(ctx, *job); err != nil {
		t.Fatal(err)
	}
	// Compaction is cumulative: the bot receives its current memory and the
	// new messages as data, with its own prompt and no added instructions.
	if model.input[0].Content != "You are a personal assistant."+e.runtimeContext(scope.BotID) && !strings.HasPrefix(model.input[0].Content, "You are a personal assistant.\n\n## Runtime") {
		t.Fatalf("compaction does not use the bot prompt: %q", model.input[0].Content)
	}
	if !strings.Contains(model.input[1].Content, "## Current memory\ncompact memory\n## Reactions to your messages\n") ||
		!strings.Contains(model.input[1].Content, "## New messages\n") || !strings.Contains(model.input[1].Content, `{"type":"memory_compaction"}`) {
		t.Fatalf("previous summary not supplied to the compaction model: %s", model.input[1].Content)
	}
	c, err = e.DB.Agent().Context(ctx, scope, 0, 200)
	if err != nil || c.Summary.ThroughID <= old {
		t.Fatal("partial compaction cannot continue")
	}
}
func TestReplyArgumentsAreValidated(t *testing.T) {
	for _, r := range []turnResult{{Name: toolReply, Arguments: `{"text":""}`}, {Name: toolReply, Arguments: `not json`}, {Prose: "   "}} {
		if _, err := replyOf(r); err == nil {
			t.Fatalf("invalid terminal result accepted: %+v", r)
		}
	}
	if got, err := replyOf(turnResult{Name: toolSkip, Arguments: `{}`}); err != nil || !got.Skip {
		t.Fatal("skip not recognised")
	}
}

func TestEditedMessageDoesNotInvalidateMemory(t *testing.T) {
	// Memory is the past: editing a bot message must not delete the existing
	// background episode. memory_revision still advances so a stale compaction
	// job is rejected.
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
	if err := e.CompactBackground(ctx, *first); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, first.Receipt); err != nil {
		t.Fatal(err)
	}
	revBefore, _ := e.DB.Agent().MemoryRevision(ctx, scope)
	if err := e.DB.Agent().SyncMessage(ctx, scope, 10001, "corrected answer", false); err != nil {
		t.Fatal(err)
	}
	c, err := e.DB.Agent().Context(ctx, scope, 0, 200)
	if err != nil || c.Summary.Content != "old summary" {
		t.Fatal("edit must not invalidate existing memory", err)
	}
	revAfter, _ := e.DB.Agent().MemoryRevision(ctx, scope)
	if revAfter <= revBefore {
		t.Fatal("memory_revision did not advance after edit")
	}
}

// When a task run sends a reply after message M, the queued respond job for M
// must skip to avoid a duplicate answer in the same chat.
// This applies to group chats with group_response="all", where messages that
// don't explicitly address the bot (payload.Addressed=false) still get a
// respond job. A task run seeing those messages should suppress the respond job.
func TestRespondJobSkippedIfTaskAlreadyReplied(t *testing.T) {
	e, _, sender, scope := engineFixture(t)
	ctx := context.Background()

	// Seed: Accept a silent group message to create the chat_directory row (no
	// respond job because GroupResponse defaults to "mentions" and Respond is false).
	if err := e.DB.Agent().Accept(ctx, agent.Incoming{
		Scope: scope, UpdateID: 999, MessageID: 999, UserID: 7,
		ChatType: "group", Text: "seed", SentAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Configure group_response="all" so Accept produces an unaddressed respond job
	// (Addressed=false in payload). That is the scenario where task-run dedup fires.
	b, err := e.DB.Agent().ChatBehavior(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	b.GroupResponse = "all"
	if err := e.DB.Agent().SetChatBehavior(ctx, b); err != nil {
		t.Fatal(err)
	}

	// Enqueue the task-style respond job first so the queue assigns it a lower ID
	// and claims it before the respond job for M.
	taskPayload, _ := json.Marshal(agent.JobPayload{Prompt: "daily reminder", TaskID: "task-1"})
	if _, err := e.DB.Queue().Enqueue(ctx, queue.Input{
		Scope: scope, ActorUserID: 7, Key: "task:task-1:1:0", Kind: "respond",
		Payload: taskPayload, MaxAttempts: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// Accept a group message; with group_response="all" and Respond=false (default),
	// Accept overrides Respond to true but keeps Addressed=false in the job payload.
	if err := e.DB.Agent().Accept(ctx, agent.Incoming{
		Scope: scope, UpdateID: 1, MessageID: 1, UserID: 7,
		ChatType: "group", Text: "question", SentAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Run the task job (lower ID → claimed first). It inserts a bot reply after M.
	taskJob := take(t, e, "respond")
	var jp agent.JobPayload
	if err := json.Unmarshal(taskJob.Payload, &jp); err != nil || jp.Prompt == "" {
		t.Fatal("expected task job to be claimed first")
	}
	if err := e.Respond(ctx, *taskJob); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, taskJob.Receipt); err != nil {
		t.Fatal(err)
	}

	// The respond job for M must not send another reply.
	callsBefore := sender.calls
	respondJob := take(t, e, "respond")
	if err := e.Respond(ctx, *respondJob); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, respondJob.Receipt); err != nil {
		t.Fatal(err)
	}
	if sender.calls != callsBefore {
		t.Fatalf("respond job sent %d extra message(s) after task already replied", sender.calls-callsBefore)
	}
}

// TestRespondJobsBatchedWhenNewerPending verifies that when multiple respond
// jobs pile up in a scope, only the last one sends a reply. Earlier jobs skip
// when they detect a pending job with a higher queue ID.
func TestRespondJobsBatchedWhenNewerPending(t *testing.T) {
	e, model, sender, scope := engineFixture(t)
	ctx := context.Background()

	// Seed the chat directory with group_response="all" so each Accept from any
	// user creates an unaddressed (Addressed=false) respond job.
	if err := e.DB.Agent().Accept(ctx, agent.Incoming{
		Scope: scope, UpdateID: 999, MessageID: 999, UserID: 7,
		ChatType: "group", Text: "seed", SentAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	b, err := e.DB.Agent().ChatBehavior(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	b.GroupResponse = "all"
	if err := e.DB.Agent().SetChatBehavior(ctx, b); err != nil {
		t.Fatal(err)
	}

	// Three messages from three different users → three separate respond jobs
	// (coalescing does not merge different-user messages).
	orig := storage.CoalescingDelay
	storage.CoalescingDelay = 0
	defer func() { storage.CoalescingDelay = orig }()

	for i, uid := range []int64{7, 8, 9} {
		if err := e.DB.Agent().Accept(ctx, agent.Incoming{
			Scope: scope, UpdateID: int64(i + 1), MessageID: int64(i + 1),
			UserID: uid, ChatType: "group", Text: "hi", SentAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Jobs are claimed in queue-ID order (lowest first). The first two must
	// skip silently; the third sends the single batched reply.
	for i := 0; i < 3; i++ {
		job := take(t, e, "respond")
		if err := e.Respond(ctx, *job); err != nil {
			t.Fatal(err)
		}
		if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
			t.Fatal(err)
		}
	}

	if model.calls != 1 {
		t.Fatalf("expected 1 LLM call (last job only), got %d", model.calls)
	}
	if sender.calls != 1 {
		t.Fatalf("expected 1 message sent (last job only), got %d", sender.calls)
	}
}
