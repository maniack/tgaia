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
)

// scriptedModel replays a fixed sequence of tool turns and then serves the
// final closed-schema call. It records what it was shown.
type scriptedModel struct {
	turns      []llm.Message
	reply      string
	calls      int
	finals     int
	toolInputs [][]llm.Message
	finalInput []llm.Message
	schema     string
	choices    []string
	specs      []llm.Tool
}

func call(id, name, args string) llm.Message {
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: id, Type: "function", Function: llm.ToolFunction{Name: name, Arguments: args}}}}
}
func finish(note string) llm.Message {
	return call("finish", toolPhaseFinish, `{"note":`+quote(note)+`}`)
}
func quote(s string) string {
	data, _ := json.Marshal(s)
	return string(data)
}
func (m *scriptedModel) Complete(context.Context, []llm.Message) (string, error) {
	return m.reply, nil
}
func (m *scriptedModel) CompleteJSON(_ context.Context, in []llm.Message, schema json.RawMessage) (string, error) {
	m.finals++
	m.finalInput = in
	m.schema = string(schema)
	return m.reply, nil
}
func (m *scriptedModel) CompleteTools(ctx context.Context, in []llm.Message, specs []llm.Tool) (llm.Message, error) {
	m.toolInputs = append(m.toolInputs, append([]llm.Message(nil), in...))
	m.choices = append(m.choices, llm.ToolChoice(ctx))
	m.specs = specs
	if m.calls >= len(m.turns) {
		return finish("nothing more"), nil
	}
	turn := m.turns[m.calls]
	m.calls++
	return turn, nil
}

// Without any MCP server the built-in tools are still offered, every turn
// demands a call, and phase_finish closes the phase with a bounded note.
func TestBuiltinToolsWithoutMCP(t *testing.T) {
	e, _, sender, scope := engineFixture(t)
	ctx := context.Background()
	long := strings.Repeat("draft of the whole answer ", 40)
	m := &scriptedModel{reply: `{"reply":"У тебя нет задач."}`, turns: []llm.Message{call("c1", toolTaskList, `{}`), finish(long)}}
	e.Model = m
	accept(t, e, scope, 1, "какие у меня задачи?", true)
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if m.calls != 2 || m.finals != 1 || sender.calls != 1 {
		t.Fatalf("turns/finals/sends = %d/%d/%d", m.calls, m.finals, sender.calls)
	}
	for _, choice := range m.choices {
		if choice != "required" {
			t.Fatalf("tool turn ran with tool_choice %q", choice)
		}
	}
	names := map[string]bool{}
	for _, spec := range m.specs {
		names[spec.Function.Name] = true
	}
	for _, want := range []string{toolPhaseFinish, toolTaskList, toolTaskCreate, toolTaskUpdate, toolTaskDelete, toolMemoryList, toolMemorySave, toolMemoryDelete} {
		if !names[want] {
			t.Fatalf("built-in tool %s not offered: %v", want, names)
		}
	}
	if !strings.Contains(m.toolInputs[1][len(m.toolInputs[1])-1].Content, `"tasks":[]`) {
		t.Fatalf("task_list result missing: %s", m.toolInputs[1][len(m.toolInputs[1])-1].Content)
	}
	final := m.finalInput
	note := final[len(final)-2]
	if note.Role != "assistant" || len([]rune(note.Content)) > noteLimit+1 || !strings.HasSuffix(note.Content, "…") {
		t.Fatalf("closing note not clipped: role=%s len=%d", note.Role, len([]rune(note.Content)))
	}
	if sender.last != "У тебя нет задач." {
		t.Fatalf("delivered %q", sender.last)
	}
}

// A model that ignores tool_choice and answers in prose produces a clipped
// note, never the reply.
func TestProseTurnIsClippedToNote(t *testing.T) {
	e, _, sender, scope := engineFixture(t)
	m := &scriptedModel{reply: `{"reply":"ok"}`, turns: []llm.Message{{Role: "assistant", Content: strings.Repeat("Полный ответ пользователю. ", 50)}}}
	e.Model = m
	accept(t, e, scope, 1, "привет", true)
	job := take(t, e, "respond")
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	if m.calls != 1 || sender.last != "ok" {
		t.Fatal("prose turn was not the end of the phase or leaked")
	}
	note := m.finalInput[len(m.finalInput)-2]
	if len([]rune(note.Content)) > noteLimit+1 {
		t.Fatalf("prose note not clipped: %d", len([]rune(note.Content)))
	}
}

func TestTaskToolsRespectOwnershipAndQuota(t *testing.T) {
	e, _, _, scope := engineFixture(t)
	ctx := context.Background()
	e.Now = func() time.Time { return time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC) }
	e.Config.Limits = map[string]string{"active_tasks_per_user": "1"}
	create := func(id string) llm.Message {
		return call(id, toolTaskCreate, `{"prompt":"task `+id+`","when":{"date":"","weekday":"","time":"","timezone":"","delay_seconds":3600},"interval_seconds":0}`)
	}
	m := &scriptedModel{reply: `{"reply":"ok"}`, turns: []llm.Message{create("c1"), create("c2"), call("c3", toolTaskList, `{}`), finish("done")}}
	e.Model = m
	accept(t, e, scope, 1, "два напоминания", true)
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	second := m.toolInputs[2][len(m.toolInputs[2])-1].Content
	if !strings.Contains(second, "maximum number of active tasks") {
		t.Fatalf("quota not reported to the model: %s", second)
	}
	listed := m.toolInputs[3][len(m.toolInputs[3])-1].Content
	if !strings.Contains(listed, `"mine":true`) || strings.Count(listed, `"id":"job-`) != 1 {
		t.Fatalf("task_list: %s", listed)
	}
	if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}
	d, _ := e.DB.Agent().Dashboard(ctx)
	if len(d.Tasks) != 1 {
		t.Fatalf("tasks: %+v", d.Tasks)
	}
	id := d.Tasks[0].ID
	// Another user in the same chat cannot touch it; the owner can pause it.
	other := &scriptedModel{reply: `{"reply":"ok"}`, turns: []llm.Message{call("c1", toolTaskDelete, `{"id":"`+id+`"}`), finish("x")}}
	e.Model = other
	if err := e.DB.Agent().Accept(ctx, incomingFrom(scope, 2, 8, "удали задачу")); err != nil {
		t.Fatal(err)
	}
	job = take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(other.toolInputs[1][len(other.toolInputs[1])-1].Content, "belongs to another user") {
		t.Fatal("foreign task was deletable")
	}
	if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}
	owner := &scriptedModel{reply: `{"reply":"ok"}`, turns: []llm.Message{call("c1", toolTaskUpdate, `{"id":"`+id+`","prompt":null,"when":null,"interval_seconds":null,"active":false}`), finish("x")}}
	e.Model = owner
	accept(t, e, scope, 3, "поставь на паузу", true)
	job = take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	d, _ = e.DB.Agent().Dashboard(ctx)
	if len(d.Tasks) != 1 || d.Tasks[0].Active {
		t.Fatalf("owner could not pause: %+v", d.Tasks)
	}
}

// Secrets are refused by memory_save even if the model tries.
func TestSaveMemoryRefusesSecrets(t *testing.T) {
	e, _, _, scope := engineFixture(t)
	m := &scriptedModel{reply: `{"reply":"Нет."}`, turns: []llm.Message{call("c1", toolMemorySave, `{"content":"API key of user 7: sk-abc"}`), finish("refused")}}
	e.Model = m
	accept(t, e, scope, 1, "запомни мой ключ sk-abc", true)
	job := take(t, e, "respond")
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.toolInputs[1][len(m.toolInputs[1])-1].Content, "credential or secret") {
		t.Fatal("secret stored")
	}
	c, _ := e.DB.Agent().Context(context.Background(), scope, 0, 10)
	if len(c.ExplicitMemories) != 0 {
		t.Fatal("secret stored")
	}
}

func incomingFrom(scope chat.Scope, id, userID int64, text string) agent.Incoming {
	return agent.Incoming{Scope: scope, UpdateID: id, MessageID: id, UserID: userID, ChatType: "private", Title: "test", Text: text, SentAt: time.Now().Add(-time.Hour), Respond: true}
}
