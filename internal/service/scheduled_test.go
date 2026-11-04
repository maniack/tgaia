package service

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/queue"
)

func dueReminder(t *testing.T) (*Engine, *testModel, *testSender, *queue.Job) {
	t.Helper()
	e, model, sender, scope := engineFixture(t)
	ctx := context.Background()
	// Reproduce the screenshot, including the earlier acknowledgement in history.
	// The task itself is created through the create_task tool.
	creator := &scriptedModel{reply: `{"reply":"Хорошо, я напомню тебе выпить воды через 5 минут."}`, turns: []llm.Message{
		call("c1", toolCreateTask, `{"prompt":"Напомни мне выпить воды сейчас","when":{"date":"","weekday":"","time":"","timezone":"","delay_seconds":300},"interval_seconds":0}`),
		finish("Task created; acknowledge briefly."),
	}}
	e.Model = creator
	accept(t, e, scope, 1, "Напомни мне через 5 минут выпить воды", true)
	first := take(t, e, "respond")
	if err := e.Respond(ctx, *first); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, first.Receipt); err != nil {
		t.Fatal(err)
	}
	e.Model = model
	if err := e.DB.Agent().Tick(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	return e, model, sender, take(t, e, "respond")
}

type scheduledModel struct {
	testModel
	schema json.RawMessage
}

func (m *scheduledModel) CompleteJSON(ctx context.Context, in []llm.Message, schema json.RawMessage) (string, error) {
	m.schema = schema
	return m.Complete(ctx, in)
}
func TestScheduledExecutionModeAndNoReschedule(t *testing.T) {
	e, _, sender, job := dueReminder(t)
	ctx := context.Background()
	// During scheduled execution the model tries to create another task; the
	// mutating tools are not offered and a call to one is refused.
	m := &scriptedModel{reply: `{"reply":"Пора выпить воды!"}`, turns: []llm.Message{
		call("c1", toolCreateTask, `{"prompt":"unwanted repeat","when":{"date":"","weekday":"","time":"","timezone":"","delay_seconds":300},"interval_seconds":0}`),
		finish("Reminder due."),
	}}
	e.Model = m
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	for _, spec := range m.specs {
		if spec.Function.Name == toolCreateTask || spec.Function.Name == toolSaveMemory {
			t.Fatal("scheduled execution offers task or memory mutation tools")
		}
	}
	if !strings.Contains(m.toolInputs[1][len(m.toolInputs[1])-1].Content, "not available during scheduled execution") {
		t.Fatal("mutating tool call during scheduled execution was not refused")
	}
	if !strings.Contains(m.finalInput[0].Content, "scheduled time has ALREADY ARRIVED") || !strings.Contains(m.finalInput[1].Content, "Scheduled execution is due NOW") || !strings.Contains(m.finalInput[1].Content, "Хорошо, я напомню") {
		t.Fatal("execution mode/history regression")
	}
	var schema struct {
		Properties map[string]struct{ Type string }
	}
	if json.Unmarshal([]byte(m.schema), &schema) != nil || len(schema.Properties) != 1 || schema.Properties["reply"].Type != "string" {
		t.Fatalf("final schema must be reply-only: %s", m.schema)
	}
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 2 || m.finals != 1 {
		t.Fatal("retry repeated reminder")
	}
	state, err := e.DB.Agent().Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Tasks) != 1 || state.Tasks[0].Active {
		t.Fatal("scheduled execution created or reactivated a task")
	}
}

func TestLiveScheduledReminder(t *testing.T) {
	endpoint, model := os.Getenv("TEST_LLM_ENDPOINT"), os.Getenv("TEST_LLM_MODEL")
	if endpoint == "" || model == "" {
		t.Skip("set TEST_LLM_ENDPOINT and TEST_LLM_MODEL for opt-in model verification")
	}
	e, _, _, job := dueReminder(t)
	client, err := llm.New(endpoint, model, config.NewSecret(os.Getenv("TEST_LLM_API_KEY")))
	if err != nil {
		t.Fatal(err)
	}
	e.Model = client
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	if err = e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	result, err := e.DB.Agent().Result(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var response storedResult
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(response.Reply)
	if !strings.Contains(lower, "вод") || strings.Contains(lower, "напомню") || strings.Contains(lower, "через 5") {
		t.Fatalf("not an immediate reminder: %s", response.Reply)
	}
	t.Logf("Scheduled delivery: %s", response.Reply)
}
