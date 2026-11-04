package queue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type testStore struct {
	mu        sync.Mutex
	taken     bool
	extends   int
	completed int
	retries   int
	failed    int
	delay     time.Duration
	loseLease bool
	settled   chan struct{}
	renewed   chan struct{}
}

func (s *testStore) Claim(ctx context.Context, kinds []string, lease time.Duration) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.taken {
		return nil, nil
	}
	s.taken = true
	return &Job{Receipt: Receipt{ID: 1, Token: "token"}, Kind: "test", Attempt: 2}, nil
}
func (s *testStore) Extend(ctx context.Context, r Receipt, lease time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extends++
	if s.loseLease {
		return ErrLeaseLost
	}
	if s.extends == 2 && s.renewed != nil {
		close(s.renewed)
	}
	return nil
}
func (s *testStore) Complete(ctx context.Context, r Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed++
	close(s.settled)
	return nil
}
func (s *testStore) Retry(ctx context.Context, r Receipt, delay time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retries++
	s.delay = delay
	close(s.settled)
	return nil
}
func (s *testStore) Fail(ctx context.Context, r Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed++
	close(s.settled)
	return nil
}
func options() Options {
	o := DefaultOptions()
	o.Concurrency = 1
	o.PollInterval = time.Millisecond
	o.Lease = 300 * time.Millisecond
	o.Heartbeat = 20 * time.Millisecond
	o.TaskTimeout = time.Second
	o.ShutdownTimeout = time.Second
	return o
}
func worker(t *testing.T, s *testStore, h Handler, o Options) *Worker {
	t.Helper()
	w, err := NewWorker(s, map[string]Handler{"test": h}, o, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
	}
}
func TestWorkerSettlement(t *testing.T) {
	for _, mode := range []string{"success", "failure", "panic", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			store := &testStore{settled: make(chan struct{})}
			handler := func(ctx context.Context, job Job) error {
				switch mode {
				case "failure":
					return errors.New("secret error")
				case "panic":
					panic("secret panic")
				case "timeout":
					<-ctx.Done()
					return nil
				}
				return nil
			}
			o := options()
			if mode == "timeout" {
				o.TaskTimeout = 30 * time.Millisecond
			}
			w := worker(t, store, handler, o)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- w.Run(ctx) }()
			await(t, store.settled)
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if mode == "success" {
				if store.completed != 1 || store.retries != 0 {
					t.Fatal("success not completed")
				}
			} else {
				if store.completed != 0 || store.retries != 1 || store.delay != 2*o.RetryBase {
					t.Fatal("failure did not back off")
				}
			}
		})
	}
}
func TestWorkerTerminalFailureRetiresJob(t *testing.T) {
	store := &testStore{settled: make(chan struct{})}
	w := worker(t, store, func(context.Context, Job) error {
		return WithPhase(Fail("llm_timeout", true, errors.New("secret detail")), "tool_turn:0")
	}, options())
	dead := make(chan string, 1)
	w.OnDead(func(_ context.Context, job Job, class string) { dead <- class })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	await(t, store.settled)
	select {
	case class := <-dead:
		if class != "llm_timeout" {
			t.Fatalf("unexpected class %q", class)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dead notification missing")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failed != 1 || store.retries != 0 || store.completed != 0 {
		t.Fatalf("terminal failure not retired: failed=%d retries=%d", store.failed, store.retries)
	}
}
func TestClassify(t *testing.T) {
	if class, terminal := Classify(errors.New("x")); class != "handler_error" || terminal {
		t.Fatal("plain errors must retry as handler_error")
	}
	if class, terminal := Classify(context.DeadlineExceeded); class != "job_timeout" || terminal {
		t.Fatal("job deadline must retry as job_timeout")
	}
	err := WithPhase(Fail("llm_429", false, nil), "final")
	if class, terminal := Classify(err); class != "llm_429" || terminal || Phase(err) != "final" {
		t.Fatal("classified failure lost its class or phase")
	}
	if class, _ := Classify(Fail("Bad Class!", true, nil)); class != "other" {
		t.Fatal("unbounded class names must collapse to other")
	}
	if (Job{Attempt: 3, MaxAttempts: 3}).Exhausted() != true || (Job{Attempt: 3}).Exhausted() {
		t.Fatal("exhaustion must require a reported attempt budget")
	}
}
func TestWorkerHeartbeat(t *testing.T) {
	store := &testStore{settled: make(chan struct{}), renewed: make(chan struct{})}
	w := worker(t, store, func(ctx context.Context, job Job) error {
		select {
		case <-store.renewed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, options())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	await(t, store.settled)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.extends < 2 || store.completed != 1 {
		t.Fatal("lease not renewed")
	}
}
func TestWorkerLosesLease(t *testing.T) {
	store := &testStore{loseLease: true, settled: make(chan struct{})}
	cancelled := make(chan struct{})
	w := worker(t, store, func(ctx context.Context, job Job) error { <-ctx.Done(); close(cancelled); return ctx.Err() }, options())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	await(t, cancelled)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.completed != 0 || store.retries != 0 {
		t.Fatal("lost owner settled work")
	}
}
func TestWorkerShutdown(t *testing.T) {
	store := &testStore{settled: make(chan struct{})}
	started := make(chan struct{})
	w := worker(t, store, func(ctx context.Context, job Job) error { close(started); <-ctx.Done(); return ctx.Err() }, options())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	await(t, started)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.completed != 0 || store.retries != 0 {
		t.Fatal("shutdown should leave lease for recovery")
	}
}
func TestWorkerShutdownDeadline(t *testing.T) {
	store := &testStore{settled: make(chan struct{})}
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	o := options()
	o.ShutdownTimeout = 10 * time.Millisecond
	w := worker(t, store, func(ctx context.Context, job Job) error { close(started); <-release; return nil }, o)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	await(t, started)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected fatal shutdown timeout")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown unbounded")
	}
}
func TestWorkerBackoffBounded(t *testing.T) {
	w := worker(t, &testStore{}, func(context.Context, Job) error { return nil }, options())
	if w.backoff(100) != w.opts.RetryMax {
		t.Fatal("retry cap ignored")
	}
}
