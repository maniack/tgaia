// Package queue defines durable jobs and their processing contracts without ORM types.
package queue

import (
	"context"
	"errors"
	"github.com/maniack/tgaia/internal/chat"
	"time"
)

var ErrLeaseLost = errors.New("queue lease lost")

type Scope = chat.Scope
type Input struct {
	Scope        Scope
	SenderChatID int64
	ActorUserID  int64  // Originating user; zero only for trusted system work.
	Key          string // Stable event/occurrence key, unique within scope; never a random retry ID.
	Kind         string
	Payload      []byte
	MaxAttempts  int
}
type Receipt struct {
	ID    int64
	Token string
}
type Job struct {
	Receipt
	Scope        Scope
	SenderChatID int64
	ActorUserID  int64
	Kind         string
	Payload      []byte
	Attempt      int
	MaxAttempts  int // Zero when the store predates attempt reporting.
}

// Exhausted reports whether this attempt was the job's last one.
func (j Job) Exhausted() bool { return j.MaxAttempts > 0 && j.Attempt >= j.MaxAttempts }

type Store interface {
	Claim(context.Context, []string, time.Duration) (*Job, error)
	Extend(context.Context, Receipt, time.Duration) error
	Complete(context.Context, Receipt) error
	Retry(context.Context, Receipt, time.Duration) error
	// Fail retires the job immediately without consuming the remaining attempts.
	Fail(context.Context, Receipt) error
}

type Handler func(context.Context, Job) error
