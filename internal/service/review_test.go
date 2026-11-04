package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/mcpclient"
)

func acceptFrom(t *testing.T, e *Engine, scope chat.Scope, id, user int64, name, username, text string) {
	t.Helper()
	if err := e.DB.Agent().Accept(context.Background(), agent.Incoming{Scope: scope, UpdateID: id, MessageID: id, UserID: user, Username: username, Name: name, ChatType: "supergroup", Title: "team", Text: text, SentAt: time.Now().Add(-2 * time.Hour), Respond: true}); err != nil {
		t.Fatal(err)
	}
}

// Every history line and the request header identify the person by name,
// @username and id, so the model can tell participants apart.
func TestPromptIdentifiesParticipants(t *testing.T) {
	e, model, _, scope := engineFixture(t)
	ctx := context.Background()
	acceptFrom(t, e, scope, 1, 7, "Иван", "ivan", "я за пиццу")
	acceptFrom(t, e, scope, 2, 8, "Мария", "maria", "а я против")
	// Names are refreshed on every update.
	acceptFrom(t, e, scope, 3, 8, "Маша", "maria_new", "решайте")
	for i := 0; i < 3; i++ {
		job := take(t, e, "respond")
		if err := e.Respond(ctx, *job); err != nil {
			t.Fatal(err)
		}
		if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
			t.Fatal(err)
		}
	}
	in := model.input[1].Content
	for _, want := range []string{`from="Иван" @ivan id=7 sender_chat=0] я за пиццу`, `from="Маша" @maria_new id=8 sender_chat=0] а я против`, `Respond to message_id=3 from "Маша" @maria_new id=8`} {
		if !strings.Contains(in, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, in)
		}
	}
	if strings.Contains(in, "user=7") || strings.Contains(in, "by user_id=") {
		t.Fatal("legacy anonymous identifiers remain in the prompt")
	}
	// Compaction input carries the same attribution and the prompt demands
	// per-person sections.
	model.reply = "Chat: пицца обсуждается.\n\nИван @ivan id=7: за пиццу.\n\nМаша @maria_new id=8: против."
	if err := e.DB.Agent().Tick(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	job := take(t, e, "compact")
	if err := e.Compact(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.input[1].Content, `from="Иван" @ivan id=7`) || !strings.Contains(model.input[0].Content, "one section per person") {
		t.Fatal("compaction is not person-attributed")
	}
}

func TestScheduleAcceptsAbsoluteFirstRun(t *testing.T) {
	e, model, _, scope := engineFixture(t)
	ctx := context.Background()
	first := time.Now().Add(49 * time.Hour).Truncate(time.Hour).In(time.FixedZone("MSK", 3*3600))
	raw, _ := json.Marshal(map[string]any{"reply": "Запланировал тост на четверг, 19:00 МСК.", "schedule": map[string]any{"prompt": "Напиши тост для команды", "first_run_at": first.Format(time.RFC3339), "delay_seconds": 0, "interval_seconds": 604800}, "remember": nil})
	model.reply = string(raw)
	accept(t, e, scope, 1, "запланируй каждый четверг в 19 по МСК тост", true)
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	d, err := e.DB.Agent().Dashboard(ctx)
	if err != nil || len(d.Tasks) != 1 {
		t.Fatal("weekly task not created", err)
	}
	if d.Tasks[0].RunAt != first.UnixMilli() || d.Tasks[0].IntervalMS != 604800*1000 {
		t.Fatalf("absolute first run not honored: run_at=%d want %d interval=%d", d.Tasks[0].RunAt, first.UnixMilli(), d.Tasks[0].IntervalMS)
	}
	// Out-of-range or malformed timestamps are rejected like any invalid schedule.
	for _, bad := range []string{"next thursday", time.Now().Add(-2 * time.Hour).Format(time.RFC3339), time.Now().Add(400 * 24 * time.Hour).Format(time.RFC3339)} {
		raw, _ := json.Marshal(map[string]any{"reply": "x", "schedule": map[string]any{"prompt": "p", "first_run_at": bad, "delay_seconds": 0, "interval_seconds": 0}, "remember": nil})
		if _, err := decodeResponse(string(raw)); err == nil {
			t.Fatalf("invalid first_run_at %q accepted", bad)
		}
	}
	if _, err := decodeResponse(`{"reply":"x","schedule":{"prompt":"p","delay_seconds":300,"interval_seconds":0},"remember":null}`); err != nil {
		t.Fatal("relative schedules must still work", err)
	}
}

type notingFixture struct {
	finalSystem string
	toolSystem  string
	turnBudget  int
}

func (f *notingFixture) Open(context.Context, chat.Scope) (mcpclient.Session, error) { return f, nil }
func (f *notingFixture) Tools() []llm.Tool {
	return []llm.Tool{{Type: "function", Function: llm.ToolFunction{Name: "lookup"}}}
}
func (f *notingFixture) Validate(string, string) error                        { return nil }
func (f *notingFixture) Call(context.Context, string, string) (string, error) { return "{}", nil }
func (f *notingFixture) Close()                                               {}
func (f *notingFixture) Complete(context.Context, []llm.Message) (string, error) {
	return "", nil
}
func (f *notingFixture) CompleteJSON(_ context.Context, in []llm.Message, _ json.RawMessage) (string, error) {
	f.finalSystem = in[0].Content
	return `{"reply":"ok","schedule":null,"remember":null}`, nil
}
func (f *notingFixture) CompleteTools(ctx context.Context, in []llm.Message, _ []llm.Tool) (llm.Message, error) {
	f.toolSystem = in[0].Content
	f.turnBudget = llm.OutputBudget(ctx)
	return llm.Message{Role: "assistant", Content: "Note: the user wants a weekly reminder; no tool needed."}, nil
}

// With tools available but unused, the final call still gets the response
// protocol, the tool phase knows that scheduling/memory are not tools, and tool
// turns run under their own output budget.
func TestFinalCallKeepsProtocolWhenToolsAreAvailable(t *testing.T) {
	e, _, _, scope := engineFixture(t)
	f := &notingFixture{}
	e.Tools = f
	e.Model = f
	accept(t, e, scope, 1, "напоминай каждый четверг", true)
	job := take(t, e, "respond")
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.finalSystem, "Output ONLY one JSON object") || !strings.Contains(f.finalSystem, "first_run_at") || strings.Contains(f.finalSystem, "ReAct") {
		t.Fatal("final call lost the response protocol or kept tool-phase guidance")
	}
	if !strings.Contains(f.toolSystem, "are not tools and need no tool call") || !strings.Contains(f.toolSystem, "never tell the user that you lack a scheduler") {
		t.Fatal("tool phase does not know that scheduling and memory exist")
	}
	if f.turnBudget != e.Config.Limit("tool_output_tokens") {
		t.Fatalf("tool turn budget %d, want %d", f.turnBudget, e.Config.Limit("tool_output_tokens"))
	}
}

func TestBoundSummary(t *testing.T) {
	long := strings.Repeat("Факт один. ", 300) + "\n\nВторой абзац. " + strings.Repeat("ещё ", 200)
	bounded := boundSummary(long, 1000)
	if n := len([]rune(bounded)); n > 1000 || n < 500 {
		t.Fatalf("unexpected bounded length %d", n)
	}
	if !strings.HasSuffix(bounded, ".") {
		t.Fatalf("cut not at a boundary: %q", bounded[len(bounded)-20:])
	}
	if boundSummary("short", 1000) != "short" {
		t.Fatal("short summaries must be unchanged")
	}
}

func TestEditWithoutTextChangeIsIgnored(t *testing.T) {
	e, model, sender, scope := engineFixture(t)
	ctx := context.Background()
	accept(t, e, scope, 1, "hello", true)
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}
	// Same text, newer update: Telegram re-sent the message (link preview etc.).
	if err := e.DB.Agent().Accept(ctx, agent.Incoming{Scope: scope, MessageID: 1, UpdateID: 2, UserID: 7, Text: "hello", Edited: true, Respond: true, SentAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if next, err := e.DB.Queue().Claim(ctx, []string{"respond"}, time.Minute); err != nil || next != nil {
		t.Fatal("unchanged edit produced a new response job")
	}
	if model.calls != 1 || sender.calls != 1 {
		t.Fatal("unchanged edit reached the model or the chat")
	}
}
