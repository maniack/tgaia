package service

import (
	"context"
	"strings"
	"testing"

	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/queue"
)

// truncatingModel fails the listed tool-turn indexes with llm_truncated and
// records whether each turn ran with thinking explicitly disabled.
type truncatingModel struct {
	scriptedModel
	fail     map[int]bool
	attempts int
	noThink  []bool
}

func (m *truncatingModel) CompleteTools(ctx context.Context, in []llm.Message, specs []llm.Tool) (llm.Message, error) {
	enabled, set := llm.Thinking(ctx)
	m.noThink = append(m.noThink, set && !enabled)
	attempt := m.attempts
	m.attempts++
	if m.fail[attempt] {
		return llm.Message{}, queue.Fail(llm.ClassTruncated, true, nil)
	}
	return m.scriptedModel.CompleteTools(ctx, in, specs)
}

// A truncated tool turn is retried once without thinking instead of killing the job.
func TestTruncatedToolTurnRetriesWithoutThinking(t *testing.T) {
	e, _, sender, scope := engineFixture(t)
	m := &truncatingModel{scriptedModel: scriptedModel{reply: `{"reply":"Задач нет."}`, turns: []llm.Message{call("c1", toolTaskList, `{}`), finish("")}}, fail: map[int]bool{0: true}}
	e.Model = m
	accept(t, e, scope, 1, "какие у меня задачи?", true)
	job := take(t, e, "respond")
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	if m.attempts != 3 || m.noThink[0] || !m.noThink[1] || m.finals != 1 || sender.calls != 1 {
		t.Fatalf("attempts=%d noThink=%v finals=%d sends=%d", m.attempts, m.noThink, m.finals, sender.calls)
	}
}

// When the retry is truncated as well, the phase closes and the reply is still written.
func TestTwiceTruncatedToolTurnClosesPhase(t *testing.T) {
	e, _, sender, scope := engineFixture(t)
	m := &truncatingModel{scriptedModel: scriptedModel{reply: `{"reply":"Не успел проверить."}`}, fail: map[int]bool{0: true, 1: true}}
	e.Model = m
	accept(t, e, scope, 1, "проверь", true)
	job := take(t, e, "respond")
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	if m.attempts != 2 || m.finals != 1 || sender.calls != 1 {
		t.Fatalf("attempts=%d finals=%d sends=%d", m.attempts, m.finals, sender.calls)
	}
}

// Rounds that execute nothing (unknown tools) end the phase after two in a row.
func TestRoundsWithoutExecutionCloseThePhase(t *testing.T) {
	e, _, sender, scope := engineFixture(t)
	m := &scriptedModel{reply: `{"reply":"Не получилось."}`, turns: []llm.Message{call("c1", "k8s_list", `{}`), call("c2", "k8s_lst", `{}`), call("c3", "k8s_ls", `{}`), call("c4", "k8s_l", `{}`)}}
	e.Model = m
	accept(t, e, scope, 1, "проверь", true)
	job := take(t, e, "respond")
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	if m.calls != 2 || m.finals != 1 || sender.calls != 1 {
		t.Fatalf("turns=%d finals=%d sends=%d", m.calls, m.finals, sender.calls)
	}
}

// Without a tool phase the final prompt forbids claiming any lookup.
func TestNoToolPhaseForbidsVerificationClaims(t *testing.T) {
	e, model, _, scope := engineFixture(t) // testModel has no tool support.
	accept(t, e, scope, 1, "проверь статус", true)
	job := take(t, e, "respond")
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(context.Background(), job.Receipt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.input[0].Content, "No tools were used for this reply") {
		t.Fatal("final prompt lacks the no-tools rule")
	}
	m := &scriptedModel{reply: `{"reply":"ok"}`}
	e.Model = m
	accept(t, e, scope, 2, "ещё раз", true)
	if err := e.Respond(context.Background(), *take(t, e, "respond")); err == nil && strings.Contains(m.finalInput[0].Content, "No tools were used") {
		t.Fatal("no-tools rule added although a tool phase ran")
	}
}
