// Package worker executes durable jobs behind a leased, fenced store boundary.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

const (
	defaultLease          = 30 * time.Second
	defaultHeartbeat      = 10 * time.Second
	defaultIdleDelay      = time.Second
	defaultRetryBaseDelay = time.Second
	defaultRetryMaxDelay  = time.Minute
)

// Store is the durable lease boundary required by Runner.
type Store interface {
	ClaimJob(context.Context, string, time.Time, time.Duration) (sqlite.Job, error)
	HeartbeatJob(context.Context, string, string, int64, time.Time, time.Duration) error
	CompleteJob(context.Context, string, string, int64, time.Time) error
	FailJob(context.Context, string, string, int64, time.Time, time.Time, bool, string, string) error
}

// Handler executes the business work for one claimed job.
type Handler interface {
	Handle(context.Context, sqlite.Job) error
}

// HandlerFunc adapts a function to a Handler.
type HandlerFunc func(context.Context, sqlite.Job) error

// Handle executes f.
func (f HandlerFunc) Handle(ctx context.Context, job sqlite.Job) error { return f(ctx, job) }

// Runner claims and executes durable jobs. It has no publication dependencies.
type Runner struct {
	Store   Store
	Handler Handler
	Owner   string

	Lease             time.Duration
	HeartbeatInterval time.Duration
	IdleDelay         time.Duration
	RetryBaseDelay    time.Duration
	RetryMaxDelay     time.Duration

	// Now and Sleep make timing deterministic in tests. Nil values use time.Now
	// and a context-aware timer.
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

// ProcessOne claims and runs at most one job. It reports false when no job is ready.
func (r *Runner) ProcessOne(ctx context.Context) (bool, error) {
	if err := r.validate(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	job, err := r.Store.ClaimJob(ctx, r.Owner, r.now(), r.Lease)
	if err != nil {
		if errors.Is(err, sqlite.ErrNoJob) {
			return false, nil
		}
		return false, fmt.Errorf("claim job: %w", err)
	}

	workCtx, cancelWork := context.WithCancel(ctx)
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	heartbeatErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go r.heartbeat(heartbeatCtx, cancelWork, job, heartbeatErr, heartbeatDone)
	slog.Default().Info("job started", "job_id", job.ID, "kind", job.Kind, "attempt", job.Attempt)

	handlerErr := r.Handler.Handle(workCtx, job)
	cancelWork()
	cancelHeartbeat()
	<-heartbeatDone

	select {
	case err := <-heartbeatErr:
		// The fence may already be gone, so deliberately do not complete or fail.
		return true, fmt.Errorf("heartbeat job %s: %w", job.ID, err)
	default:
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if handlerErr == nil {
		if err := r.Store.CompleteJob(ctx, job.ID, r.Owner, job.LeaseGeneration, r.now()); err != nil {
			return true, fmt.Errorf("complete job %s: %w", job.ID, err)
		}
		slog.Default().Info("job completed", "job_id", job.ID, "kind", job.Kind, "attempt", job.Attempt)
		return true, nil
	}

	permanent := IsPermanent(handlerErr)
	retry := !permanent && job.Attempt < job.MaxAttempts
	errorClass := "transient"
	if permanent {
		errorClass = "permanent"
	}
	now := r.now()
	if err := r.Store.FailJob(
		ctx,
		job.ID,
		r.Owner,
		job.LeaseGeneration,
		now,
		now.Add(r.retryDelay(job.Attempt)),
		retry,
		errorClass,
		handlerErr.Error(),
	); err != nil {
		return true, fmt.Errorf("fail job %s: %w", job.ID, err)
	}
	slog.Default().Warn("job failed", "job_id", job.ID, "kind", job.Kind, "attempt", job.Attempt, "retry", retry, "error_class", errorClass, "error", handlerErr)
	return true, nil
}

// Run processes ready work until ctx is canceled.
func (r *Runner) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		processed, err := r.ProcessOne(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if processed {
			continue
		}
		if err := r.sleep(ctx, r.IdleDelay); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("wait for work: %w", err)
		}
	}
}

func (r *Runner) heartbeat(
	ctx context.Context,
	cancelWork context.CancelFunc,
	job sqlite.Job,
	errorsCh chan<- error,
	done chan<- struct{},
) {
	defer close(done)
	timer := time.NewTimer(r.HeartbeatInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := r.Store.HeartbeatJob(ctx, job.ID, r.Owner, job.LeaseGeneration, r.now(), r.Lease); err != nil {
				select {
				case errorsCh <- err:
				default:
				}
				cancelWork()
				return
			}
			timer.Reset(r.HeartbeatInterval)
		}
	}
}

func (r *Runner) validate() error {
	if r.Store == nil {
		return errors.New("worker store is required")
	}
	if r.Handler == nil {
		return errors.New("worker handler is required")
	}
	if r.Owner == "" {
		return errors.New("worker owner is required")
	}
	r.defaults()
	if r.Lease <= 0 {
		return errors.New("worker lease must be positive")
	}
	if r.HeartbeatInterval <= 0 || r.HeartbeatInterval >= r.Lease {
		return errors.New("worker heartbeat interval must be positive and shorter than lease")
	}
	if r.IdleDelay <= 0 || r.RetryBaseDelay <= 0 || r.RetryMaxDelay <= 0 {
		return errors.New("worker delays must be positive")
	}
	return nil
}

func (r *Runner) defaults() {
	if r.Lease == 0 {
		r.Lease = defaultLease
	}
	if r.HeartbeatInterval == 0 {
		r.HeartbeatInterval = defaultHeartbeat
	}
	if r.IdleDelay == 0 {
		r.IdleDelay = defaultIdleDelay
	}
	if r.RetryBaseDelay == 0 {
		r.RetryBaseDelay = defaultRetryBaseDelay
	}
	if r.RetryMaxDelay == 0 {
		r.RetryMaxDelay = defaultRetryMaxDelay
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Runner) sleep(ctx context.Context, delay time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Runner) retryDelay(attempt int) time.Duration {
	delay := r.RetryBaseDelay
	if delay > r.RetryMaxDelay {
		return r.RetryMaxDelay
	}
	for i := 1; i < attempt && delay < r.RetryMaxDelay; i++ {
		if delay > r.RetryMaxDelay/2 {
			return r.RetryMaxDelay
		}
		delay *= 2
	}
	return delay
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks an error as non-retryable while preserving errors.Is/As.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool {
	var target permanentError
	return errors.As(err, &target)
}
