package service

import (
	"context"
	"errors"
	"github.com/maniack/tgaia/internal/llm"
	"sync/atomic"
	"testing"
	"time"
)

func TestTypingRefreshAndJoinedStop(t *testing.T) {
	var calls atomic.Int32
	refreshed := make(chan struct{})
	stop := typingLoop(context.Background(), 5*time.Millisecond, func(ctx context.Context) error {
		if calls.Add(1) == 2 {
			close(refreshed)
			<-ctx.Done()
		}
		return ctx.Err()
	})
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("typing was not refreshed")
	}
	stop()
	stop()
	if calls.Load() != 2 {
		t.Fatal("unexpected notification count")
	}
}
func TestTypingFailureAndCancelledParent(t *testing.T) {
	failed := make(chan struct{})
	var calls atomic.Int32
	stop := typingLoop(context.Background(), time.Millisecond, func(context.Context) error { calls.Add(1); close(failed); return errors.New("unavailable") })
	<-failed
	stop()
	if calls.Load() != 1 {
		t.Fatal("typing error retried")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stop = typingLoop(ctx, time.Millisecond, func(context.Context) error { t.Error("notification after cancellation"); return nil })
	stop()
}

type typingFixture struct {
	testSender
	started chan struct{}
	calls   atomic.Int32
}

func (s *typingFixture) Typing(ctx context.Context, chatID int64) error {
	if chatID != 100 {
		return errors.New("wrong scope")
	}
	if s.calls.Add(1) == 1 {
		close(s.started)
	}
	return nil
}

type waitTypingModel struct{ started <-chan struct{} }

func (m waitTypingModel) Complete(ctx context.Context, _ []llm.Message) (string, error) {
	select {
	case <-m.started:
		return `{"reply":"ready","schedule":null}`, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
func TestResponseStartsTypingAndCachedRetryDoesNot(t *testing.T) {
	e, _, _, scope := engineFixture(t)
	sender := &typingFixture{started: make(chan struct{})}
	e.Bots[scope.BotID] = sender
	e.Model = waitTypingModel{sender.started}
	accept(t, e, scope, 1, "hello", true)
	job := take(t, e, "respond")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if sender.calls.Load() != 1 || sender.testSender.calls != 1 {
		t.Fatal("typing or delivery repeated on cached retry")
	}
}
