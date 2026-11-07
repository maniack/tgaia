package queue

import (
	"context"
	"errors"
	"regexp"
)

// Failure classifies a handler error for retry decisions, logs and metrics.
// Class names form a small, fixed vocabulary; the wrapped error text is never
// persisted or logged because it may carry prompts or secrets.
type Failure struct {
	Class    string
	Terminal bool // A deterministic failure: retrying the same input cannot succeed.
	Phase    string
	// Detail is a short, code-built diagnostic (status codes, provider error
	// identifiers) that is safe to log. Never put model or user text here.
	Detail string
	Err    error
}

func (f *Failure) Error() string {
	if f.Err != nil {
		return f.Err.Error()
	}
	return f.Class
}
func (f *Failure) Unwrap() error { return f.Err }

// Fail wraps err with a failure class. Terminal failures retire the job at once.
func Fail(class string, terminal bool, err error) error {
	if err == nil {
		err = errors.New(class)
	}
	return &Failure{Class: class, Terminal: terminal, Err: err}
}

// WithPhase annotates err with the job phase that produced it (tool turn, final
// call, embedding). Existing failure classes are preserved.
func WithPhase(err error, phase string) error {
	if err == nil {
		return nil
	}
	var f *Failure
	if errors.As(err, &f) {
		if f.Phase == "" {
			f.Phase = phase
		}
		return err
	}
	class, terminal := Classify(err)
	return &Failure{Class: class, Terminal: terminal, Phase: phase, Err: err}
}

var classPattern = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// Classify maps any handler error to a bounded class and retry decision.
// Unknown errors retry with backoff, as before.
func Classify(err error) (class string, terminal bool) {
	if err == nil {
		return "", false
	}
	var f *Failure
	if errors.As(err, &f) {
		if classPattern.MatchString(f.Class) {
			return f.Class, f.Terminal
		}
		return "other", f.Terminal
	}
	if errors.Is(err, ErrLeaseLost) {
		return "lease_lost", false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// A job-level timeout: tool turns and results are checkpointed, so a retry
		// resumes rather than repeats the work. Model-call timeouts classify
		// themselves as terminal before reaching this point.
		return "job_timeout", false
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", false
	}
	return "handler_error", false
}

// Phase returns the annotated phase, or an empty string.
// WithDetail attaches a safe diagnostic to a failure; see Failure.Detail.
func WithDetail(err error, detail string) error {
	var f *Failure
	if errors.As(err, &f) && detailPattern.MatchString(detail) {
		f.Detail = detail
	}
	return err
}

var detailPattern = regexp.MustCompile(`^[A-Za-z0-9_.:/ -]{1,160}$`)

// Detail returns the safe diagnostic attached to err, if any.
func Detail(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Detail
	}
	return ""
}

func Phase(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Phase
	}
	return ""
}
