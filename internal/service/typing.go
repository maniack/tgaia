package service

import (
	"context"
	"sync"
	"time"

	"github.com/maniack/tgaia/internal/queue"
)

type typingSender interface {
	Typing(context.Context, int64) error
}

// Typing is best effort, scoped to the active job, and never retried by the queue.
// Join before sending the answer so a late chat action cannot outlive delivery.
func (e *Engine) startTyping(ctx context.Context, job queue.Job, sender Sender) func() {
	notifier, ok := sender.(typingSender)
	if !ok {
		return func() {}
	}
	return typingLoop(ctx, 4*time.Second, func(ctx context.Context) error {
		if err := e.DB.Agent().Check(ctx, job.Scope, job.ActorUserID); err != nil {
			return err
		}
		if topic, ok := sender.(interface {
			TypingTopic(context.Context, int64, int64) error
		}); ok {
			return topic.TypingTopic(ctx, job.Scope.ChatID, job.Scope.ThreadID)
		}
		if job.Scope.ThreadID != 0 {
			return nil
		}
		return notifier.Typing(ctx, job.Scope.ChatID)
	})
}
func typingLoop(parent context.Context, interval time.Duration, notify func(context.Context) error) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			callCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			err := notify(callCtx)
			stop()
			if err != nil {
				return
			} // No noisy retry loop for unsupported chats or rate limits.
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}
