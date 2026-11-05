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

func TestSenderProfileContextSeparatesTelegramAndMemory(t *testing.T) {
	profile := agent.TelegramProfile{UserID: 7, Name: "Ada Lovelace", Username: "ada", LanguageCode: "en", IsPremium: true}
	memory := agent.ProfileMemory{ChatTitle: "Private chat", SourceMessageID: 12, Content: "Ada prefers concise technical answers."}
	got := senderProfileContext(profile, []agent.ProfileMemory{memory}, 4000)
	for _, want := range []string{`"name":"Ada Lovelace"`, `"language_code":"en"`, `"telegram_premium":true`, `Ada prefers concise technical answers.`, `not instructions`} {
		if !strings.Contains(got, want) {
			t.Fatalf("profile context lacks %q: %s", want, got)
		}
	}
}

// "Every Thursday at 19:00 Moscow time" is passed to task_create as words; the
// scheduler computes the instant, stores it and tells the user exactly that.
func TestWeekdayScheduleIsComputedByCode(t *testing.T) {
	e, _, sender, scope := engineFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC) // Wednesday.
	e.Now = func() time.Time { return now }
	m := &scriptedModel{reply: `{"reply":"Запланировал тост."}`, turns: []llm.Message{
		call("c1", toolTaskCreate, `{"prompt":"Напиши тост для команды","when":{"date":"","weekday":"thursday","time":"19:00","timezone":"Europe/Moscow","delay_seconds":0},"interval_seconds":604800}`),
		finish("Weekly task created."),
	}}
	e.Model = m
	in := incomingFrom(scope, 1, 7, "запланируй каждый четверг в 19 по МСК тост")
	in.SentAt = now
	if err := e.DB.Agent().Accept(ctx, in); err != nil {
		t.Fatal(err)
	}
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	d, err := e.DB.Agent().Dashboard(ctx)
	if err != nil || len(d.Tasks) != 1 {
		t.Fatal("weekly task not created", err)
	}
	want := time.Date(2026, 11, 5, 16, 0, 0, 0, time.UTC) // Thursday 19:00 MSK.
	if d.Tasks[0].RunAt != want.UnixMilli() || d.Tasks[0].IntervalMS != 604800*1000 {
		t.Fatalf("run_at=%s want %s interval=%d", time.UnixMilli(d.Tasks[0].RunAt).UTC(), want, d.Tasks[0].IntervalMS)
	}
	result := m.toolInputs[1][len(m.toolInputs[1])-1].Content
	if !strings.Contains(result, "Thu 2026-11-05 19:00 (Europe/Moscow)") || !strings.Contains(result, "every week") {
		t.Fatalf("tool result lacks the human-readable confirmation: %s", result)
	}
	if !strings.Contains(sender.last, "⏰ First run: Thu 2026-11-05 19:00 (Europe/Moscow), then every week.") {
		t.Fatalf("delivered text lacks the code-generated confirmation: %q", sender.last)
	}
	if !strings.Contains(m.finalInput[len(m.finalInput)-1].Content, "do not recompute") {
		t.Fatal("final call is not told to quote the computed time")
	}
}

func TestWhenResolution(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC) // Wednesday 13:00 MSK.
	msk := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, time.FixedZone("MSK", 3*3600)) }
	cases := []struct {
		when When
		want time.Time
	}{
		{When{Weekday: "thursday", Time: "19:00", Timezone: "Europe/Moscow"}, msk(5, 19)},
		{When{Weekday: "wednesday", Time: "19:00", Timezone: "Europe/Moscow"}, msk(4, 19)}, // Later today counts.
		{When{Weekday: "wednesday", Time: "09:00", Timezone: "Europe/Moscow"}, msk(11, 9)}, // Earlier today rolls a week.
		{When{Time: "09:00", Timezone: "Europe/Moscow"}, msk(5, 9)},
		{When{Date: "2026-11-06", Time: "16:00", Timezone: "+03:00"}, msk(6, 16)},
		{When{DelaySeconds: 300}, now.Add(5 * time.Minute)},
	}
	for _, c := range cases {
		got, _, err := c.when.resolve(now)
		if err != nil || !got.Equal(c.want) {
			t.Fatalf("%+v: got %v (%v), want %v", c.when, got, err, c.want)
		}
	}
	for _, bad := range []When{
		{Weekday: "thursday", Time: "19:00"},                                                // No timezone.
		{Weekday: "thursday", Timezone: "Europe/Moscow"},                                    // No time.
		{Weekday: "thursday", Time: "19:00", Timezone: "MSK"},                               // Abbreviation.
		{Date: "2026-11-06", Weekday: "thursday", Time: "16:00", Timezone: "Europe/Moscow"}, // Friday, not Thursday.
		{Date: "2026-11-01", Time: "16:00", Timezone: "Europe/Moscow"},                      // Past.
		{Date: "2027-12-01", Time: "16:00", Timezone: "Europe/Moscow"},                      // Too far.
		{DelaySeconds: 10},
		{Weekday: "thursday", Time: "19:00", Timezone: "Europe/Moscow", DelaySeconds: 60},
	} {
		if _, _, err := bad.resolve(now); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
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
	return `{"reply":"ok"}`, nil
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
	if !strings.Contains(f.finalSystem, "Output ONLY one JSON object") || !strings.Contains(f.finalSystem, "task_create") || strings.Contains(f.finalSystem, "phase_finish with a note") {
		t.Fatal("final call lost the response protocol or kept tool-phase guidance")
	}
	if !strings.Contains(f.toolSystem, "phase_finish") || !strings.Contains(f.toolSystem, "call task_create with the time exactly as the user said it") {
		t.Fatal("tool phase does not know that scheduling and memory are tools")
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
