package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/queue"
)

var reviewSeq atomic.Int64

// summaryModel handles both compaction and reviewer calls:
//   - compaction: responds to memory_save_summary with a plain summary
//   - review: responds to review_done with the given corrections
type summaryModel struct {
	corrections string   // JSON array for review_done
	systems     []string // recorded system prompts
	inputs      []string // recorded user messages
}

func (m *summaryModel) Complete(context.Context, []llm.Message) (string, error) { return "", nil }
func (m *summaryModel) CompleteTools(_ context.Context, in []llm.Message, specs []llm.Tool) (llm.Message, error) {
	m.systems = append(m.systems, in[0].Content)
	m.inputs = append(m.inputs, in[1].Content)
	for _, spec := range specs {
		if spec.Function.Name == toolSummary {
			return call("s", toolSummary, `{"summary":"memory"}`), nil
		}
		if spec.Function.Name == toolReviewDone {
			args, _ := json.Marshal(map[string]any{"corrections": json.RawMessage(m.corrections)})
			return call("r", toolReviewDone, string(args)), nil
		}
	}
	return call("r", toolReply, `{"text":"ok"}`), nil
}

func compactOnce(t *testing.T, e *Engine) {
	t.Helper()
	ctx := context.Background()
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
}

func reviewOnce(t *testing.T, e *Engine, scope queue.Scope) {
	t.Helper()
	ctx := context.Background()
	payload, _ := json.Marshal(agent.JobPayload{})
	key := fmt.Sprintf("review:test:%d", reviewSeq.Add(1))
	if _, err := e.DB.Queue().Enqueue(ctx, queue.Input{Scope: scope, Key: key, Kind: "review_behavior", Payload: payload, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	job := take(t, e, "review_behavior")
	if err := e.ReviewBehavior(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}
}

func TestCompactionProposesCorrectionsForReview(t *testing.T) {
	e, _, _, scope := engineFixture(t)
	ctx := context.Background()
	m := &summaryModel{corrections: `[{"text":"Не утверждай, что проверил, без результата tool","reason":"Ада дважды поправила"}]`}
	e.Model = m
	for i := int64(1); i <= 30; i++ {
		accept(t, e, scope, i, "сообщение", false)
	}
	compactOnce(t, e)
	// Corrections come from ReviewBehavior, not Compact.
	reviewOnce(t, e, scope)
	list, err := e.DB.Agent().Corrections(ctx, scope, "")
	if err != nil || len(list) != 1 || list[0].Status != "pending" || list[0].Reason != "Ада дважды поправила" {
		t.Fatalf("proposal not stored as pending: %+v %v", list, err)
	}
	// Pending corrections do not reach replies.
	accept(t, e, scope, 100, "привет", true)
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.systems[len(m.systems)-1], "Behaviour corrections") {
		t.Fatal("pending correction applied")
	}
	if err := e.DB.Agent().SetCorrection(ctx, scope, list[0].ID, list[0].Version, "approve"); err != nil {
		t.Fatal(err)
	}
	accept(t, e, scope, 101, "ещё", true)
	job = take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.systems[len(m.systems)-1], "## Behaviour corrections\n- Не утверждай, что проверил") {
		t.Fatalf("approved correction missing from reply data: %s", m.systems[len(m.systems)-1])
	}
}

func TestDisabledCorrectionIsNotProposedAgainAndAutoApprove(t *testing.T) {
	e, _, _, scope := engineFixture(t)
	ctx := context.Background()
	m := &summaryModel{corrections: `[{"text":"Отвечай короче","reason":"просили"}]`}
	e.Model = m
	for i := int64(1); i <= 30; i++ {
		accept(t, e, scope, i, "сообщение", false)
	}
	compactOnce(t, e)
	reviewOnce(t, e, scope)
	list, _ := e.DB.Agent().Corrections(ctx, scope, "")
	if err := e.DB.Agent().SetCorrection(ctx, scope, list[0].ID, list[0].Version, "disable"); err != nil {
		t.Fatal(err)
	}
	b, _ := e.DB.Agent().ChatBehavior(ctx, scope)
	b.AutoApproveCorrections = true
	if err := e.DB.Agent().SetChatBehavior(ctx, b); err != nil {
		t.Fatal(err)
	}
	m.corrections = `[{"text":"отвечай короче","reason":"снова"},{"text":"Пиши по-русски","reason":"чат русский"}]`
	for i := int64(31); i <= 60; i++ {
		accept(t, e, scope, i, "сообщение", false)
	}
	compactOnce(t, e)
	// Reviewer sees existing corrections (including disabled ones).
	reviewOnce(t, e, scope)
	if !strings.Contains(m.inputs[len(m.inputs)-1], "- [disabled] Отвечай короче") {
		t.Fatal("review does not see existing corrections")
	}
	list, _ = e.DB.Agent().Corrections(ctx, scope, "")
	if len(list) != 2 || list[0].Text != "Пиши по-русски" || list[0].Status != "approved" {
		t.Fatalf("auto-approve or dedup failed: %+v", list)
	}
}
