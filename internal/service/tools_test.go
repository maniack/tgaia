package service

import (
	"context"
	"errors"
	"testing"

	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/mcpclient"
)

type toolFixture struct {
	calls, plans int
	fail         bool
}

func (f *toolFixture) Open(context.Context, chat.Scope) (mcpclient.Session, error) { return f, nil }
func (f *toolFixture) Tools() []llm.Tool {
	return []llm.Tool{{Type: "function", Function: llm.ToolFunction{Name: "lookup"}}}
}
func (f *toolFixture) Validate(name, args string) error {
	if name != "lookup" {
		return errors.New("denied")
	}
	return nil
}
func (f *toolFixture) Call(context.Context, string, string) (string, error) {
	f.calls++
	if f.fail {
		return "", errors.New("timeout")
	}
	return `{"answer":"fact"}`, nil
}
func (f *toolFixture) Close() {}
func (f *toolFixture) Complete(context.Context, []llm.Message) (string, error) {
	return `{"reply":"answer","schedule":null}`, nil
}
func (f *toolFixture) CompleteTools(_ context.Context, in []llm.Message, _ []llm.Tool) (llm.Message, error) {
	f.plans++
	if in[len(in)-1].Role == "tool" {
		return llm.Message{Role: "assistant", Content: "done"}, nil
	}
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.ToolFunction{Name: "lookup", Arguments: `{}`}}}}, nil
}
func TestToolCheckpointsSurviveRetry(t *testing.T) {
	for _, fail := range []bool{false, true} {
		e, _, _, scope := engineFixture(t)
		f := &toolFixture{fail: fail}
		e.Tools = f
		e.Model = f
		accept(t, e, scope, 1, "lookup", true)
		job := take(t, e, "respond")
		ctx := context.Background()
		request := []llm.Message{{Role: "system", Content: "test"}, {Role: "user", Content: "lookup"}}
		if _, err := e.toolContext(ctx, *job, request); err != nil {
			t.Fatal(err)
		}
		plans := f.plans
		if _, err := e.toolContext(ctx, *job, request); err != nil {
			t.Fatal(err)
		}
		if f.calls != 1 || f.plans != plans {
			t.Fatal("retry repeated tool or model turn")
		}
	}
}
func TestStartedToolNeverRepeated(t *testing.T) {
	e, _, _, scope := engineFixture(t)
	f := &toolFixture{}
	e.Tools = f
	e.Model = f
	accept(t, e, scope, 1, "lookup", true)
	job := take(t, e, "respond")
	ctx := context.Background()
	turn := `{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`
	if err := e.DB.Agent().SaveToolTurn(ctx, *job, "turn:0", turn); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Agent().StartTool(ctx, *job, "call:0:0", `{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.toolContext(ctx, *job, []llm.Message{{Role: "system", Content: "test"}, {Role: "user", Content: "lookup"}}); err != nil {
		t.Fatal(err)
	}
	if f.calls != 0 {
		t.Fatal("unknown previous outcome was repeated")
	}
	step, err := e.DB.Agent().ToolStep(ctx, job.ID, "call:0:0")
	if err != nil || step.State != "uncertain" {
		t.Fatal("unknown outcome not recorded")
	}
}
