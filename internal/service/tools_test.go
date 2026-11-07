package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/mcpclient"
	"github.com/maniack/tgaia/internal/queue"
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
	return `{"reply":"answer"}`, nil
}
func (f *toolFixture) CompleteTools(_ context.Context, in []llm.Message, _ []llm.Tool) (llm.Message, error) {
	f.plans++
	if in[len(in)-1].Role == "tool" {
		return call("final", toolReply, `{"text":"answer"}`), nil
	}
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.ToolFunction{Name: "lookup", Arguments: `{}`}}}}, nil
}

// runTools runs one response loop with the engine's tools, as Respond does.
func runTools(ctx context.Context, e *Engine, job queue.Job, request []llm.Message) (turnResult, error) {
	t := turn{job: job, messages: request, terminal: []llm.Tool{replyTool}, checkpoint: true}
	if err := e.offerActions(ctx, &t, agent.JobPayload{MessageID: 1}, time.Now(), agent.ChatBehavior{}); err != nil {
		return turnResult{}, err
	}
	if t.session != nil {
		defer t.session.Close()
	}
	return e.run(ctx, t)
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
		if _, err := runTools(ctx, e, *job, request); err != nil {
			t.Fatal(err)
		}
		plans := f.plans
		if _, err := runTools(ctx, e, *job, request); err != nil {
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
	if _, err := runTools(ctx, e, *job, []llm.Message{{Role: "system", Content: "test"}, {Role: "user", Content: "lookup"}}); err != nil {
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

type reactFixture struct {
	calls            []string
	turns            int
	seenSystemPrompt string
}

func (r *reactFixture) Open(context.Context, chat.Scope) (mcpclient.Session, error) { return r, nil }
func (r *reactFixture) Tools() []llm.Tool {
	return []llm.Tool{
		{Type: "function", Function: llm.ToolFunction{Name: "search"}},
		{Type: "function", Function: llm.ToolFunction{Name: "fetch_page"}},
	}
}
func (r *reactFixture) Validate(name, args string) error { return nil }
func (r *reactFixture) Call(_ context.Context, name, args string) (string, error) {
	r.calls = append(r.calls, name+":"+args)
	if name == "search" {
		return `{"results":[{"title":"Golang Concurrency","url":"https://go.dev/doc"}]}`, nil
	}
	if name == "fetch_page" {
		return `{"content":"Full text about goroutines and channels in Go."}`, nil
	}
	return "{}", nil
}
func (r *reactFixture) Close() {}
func (r *reactFixture) Complete(context.Context, []llm.Message) (string, error) {
	return `{"reply":"Complete answer"}`, nil
}
func (r *reactFixture) CompleteTools(_ context.Context, in []llm.Message, _ []llm.Tool) (llm.Message, error) {
	r.turns++
	r.seenSystemPrompt = in[0].Content
	last := in[len(in)-1]
	if last.Role == "user" {
		return llm.Message{
			Role:             "assistant",
			ReasoningContent: "Thought: need to search for Go documentation",
			ToolCalls: []llm.ToolCall{{
				ID:   "c1",
				Type: "function",
				Function: llm.ToolFunction{
					Name:      "search",
					Arguments: `{"query":"golang concurrency"}`,
				},
			}},
		}, nil
	}
	if last.Role == "tool" && last.ToolCallID == "c1" {
		return llm.Message{
			Role:             "assistant",
			ReasoningContent: "Thought: search returned a URL, now need to fetch the full page content for full detail",
			ToolCalls: []llm.ToolCall{{
				ID:   "c2",
				Type: "function",
				Function: llm.ToolFunction{
					Name:      "fetch_page",
					Arguments: `{"url":"https://go.dev/doc"}`,
				},
			}},
		}, nil
	}
	if last.Role == "tool" && last.ToolCallID == "c2" {
		answer := call("c3", toolReply, `{"text":"Goroutines and channels."}`)
		answer.ReasoningContent = "Thought: full page content acquired, sufficient to answer."
		return answer, nil
	}
	return llm.Message{Role: "assistant", Content: "done"}, nil
}

func TestReActThinkingToolLoop(t *testing.T) {
	e, _, _, scope := engineFixture(t)
	f := &reactFixture{}
	e.Tools = f
	e.Model = f
	accept(t, e, scope, 1, "find info about golang", true)
	job := take(t, e, "respond")
	ctx := context.Background()
	request := []llm.Message{{Role: "system", Content: "Be helpful"}, {Role: "user", Content: "find info about golang"}}
	outcome, err := runTools(ctx, e, *job, request)
	if err != nil {
		t.Fatal(err)
	}
	if f.turns != 3 {
		t.Fatalf("expected 3 turns in ReAct loop, got %d", f.turns)
	}
	if len(f.calls) != 2 || f.calls[0] != `search:{"query":"golang concurrency"}` || f.calls[1] != `fetch_page:{"url":"https://go.dev/doc"}` {
		t.Fatalf("expected sequential search and fetch_page calls, got %+v", f.calls)
	}
	if f.seenSystemPrompt != "Be helpful" {
		t.Fatalf("the loop must not add instructions to the bot prompt: %q", f.seenSystemPrompt)
	}
	if reply, err := replyOf(outcome); err != nil || reply.Reply != "Goroutines and channels." {
		t.Fatalf("terminal reply = %+v %v", reply, err)
	}
}
