package queue

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"
)

type Options struct {
	Concurrency     int
	PollInterval    time.Duration
	Lease           time.Duration
	Heartbeat       time.Duration
	TaskTimeout     time.Duration
	ShutdownTimeout time.Duration
	RetryBase       time.Duration
	RetryMax        time.Duration
}

func DefaultOptions() Options {
	return Options{Concurrency: 4, PollInterval: time.Second, Lease: 30 * time.Second, Heartbeat: 10 * time.Second, TaskTimeout: 2 * time.Minute, ShutdownTimeout: 15 * time.Second, RetryBase: time.Second, RetryMax: time.Minute}
}

type Worker struct {
	store    Store
	handlers map[string]Handler
	kinds    []string
	opts     Options
	log      *slog.Logger
	dead     func(context.Context, Job, string)
}

// OnDead registers a best-effort notification that runs once, after a job was
// retired as dead by this worker (terminal failure or exhausted attempts). It
// must be set before Run and must not be used for durable work.
func (w *Worker) OnDead(fn func(ctx context.Context, job Job, class string)) { w.dead = fn }

func NewWorker(store Store, handlers map[string]Handler, opts Options, log *slog.Logger) (*Worker, error) {
	if store == nil || log == nil || len(handlers) == 0 || opts.Concurrency < 1 || opts.Concurrency > 256 || opts.PollInterval <= 0 || opts.Lease < time.Millisecond || opts.Lease > 24*time.Hour || opts.Heartbeat <= 0 || opts.Heartbeat > opts.Lease/3 || opts.TaskTimeout <= 0 || opts.ShutdownTimeout <= 0 || opts.RetryBase < time.Millisecond || opts.RetryMax < opts.RetryBase || opts.RetryMax > 24*time.Hour {
		return nil, errors.New("invalid worker configuration")
	}
	for kind, handler := range handlers {
		if kind == "" || handler == nil {
			return nil, errors.New("invalid worker handler")
		}
	}
	return &Worker{store: store, handlers: maps.Clone(handlers), kinds: slices.Sorted(maps.Keys(handlers)), opts: opts, log: log}, nil
}

// Run owns its workers. Handlers must honor context cancellation and must not
// launch detached work. Shutdown timeout is fatal: the process must then exit.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := 0; i < w.opts.Concurrency; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); w.loop(ctx) }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		timer := time.NewTimer(w.opts.ShutdownTimeout)
		defer timer.Stop()
		select {
		case <-done:
			return nil
		case <-timer.C:
			return errors.New("worker shutdown deadline exceeded")
		}
	}
}
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := w.store.Claim(ctx, w.kinds, w.opts.Lease)
		if err != nil || job == nil {
			if err != nil && ctx.Err() == nil {
				w.log.ErrorContext(ctx, "queue claim failed")
			}
			if !wait(ctx, w.opts.PollInterval) {
				return
			}
			continue
		}
		if ctx.Err() != nil {
			return
		} // Lease expires; never start new work on shutdown.
		w.execute(ctx, *job)
	}
}
func (w *Worker) execute(parent context.Context, job Job) {
	ctx, cancel := context.WithTimeout(parent, w.opts.TaskTimeout)
	defer cancel()
	stopped := make(chan struct{})
	heartbeatDone := make(chan bool, 1)
	go func() {
		ticker := time.NewTicker(w.opts.Heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-stopped:
				heartbeatDone <- true
				return
			case <-ctx.Done():
				heartbeatDone <- true
				return
			case <-ticker.C:
				renewCtx, stop := context.WithTimeout(ctx, w.opts.Heartbeat)
				err := w.store.Extend(renewCtx, job.Receipt, w.opts.Lease)
				stop()
				if err != nil {
					cancel()
					heartbeatDone <- false
					return
				}
			}
		}
	}()
	err := invoke(ctx, w.handlers[job.Kind], job)
	if ctx.Err() != nil && err == nil {
		err = ctx.Err()
	}
	close(stopped)
	owned := <-heartbeatDone
	if !owned || parent.Err() != nil {
		return
	} // Do not write after lost ownership.
	settleCtx, stop := context.WithTimeout(parent, w.opts.Heartbeat)
	defer stop()
	if err == nil {
		if err = w.store.Complete(settleCtx, job.Receipt); err != nil {
			w.log.WarnContext(parent, "queue settlement failed", "job_id", job.ID)
		}
		return
	}
	// Never persist or log raw handler errors: they may contain prompts/secrets.
	// The bounded class and phase are safe and sufficient for diagnosis.
	class, terminal := Classify(err)
	dead := terminal || job.Exhausted()
	w.log.WarnContext(parent, "queue handler failed", "job_id", job.ID, "kind", job.Kind, "attempt", job.Attempt, "class", class, "phase", Phase(err), "terminal", terminal, "dead", dead)
	if terminal {
		err = w.store.Fail(settleCtx, job.Receipt)
	} else {
		err = w.store.Retry(settleCtx, job.Receipt, w.backoff(job.Attempt))
	}
	if err != nil {
		w.log.WarnContext(parent, "queue settlement failed", "job_id", job.ID)
		return
	}
	if dead && w.dead != nil {
		notifyCtx, stopNotify := context.WithTimeout(parent, w.opts.Heartbeat)
		defer stopNotify()
		func() {
			defer func() { _ = recover() }()
			w.dead(notifyCtx, job, class)
		}()
	}
}
func invoke(ctx context.Context, h Handler, job Job) (err error) {
	defer func() {
		if recover() != nil {
			err = Fail("panic", false, errors.New("handler panic"))
		}
	}()
	return h(ctx, job)
}
func (w *Worker) backoff(attempt int) time.Duration {
	delay := w.opts.RetryBase
	for i := 1; i < attempt && delay < w.opts.RetryMax; i++ {
		if delay > w.opts.RetryMax/2 {
			return w.opts.RetryMax
		}
		delay *= 2
	}
	return min(delay, w.opts.RetryMax)
}
