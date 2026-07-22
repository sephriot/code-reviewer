// Package outbox runs fenced, durable domain-event delivery handlers.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

// Store is the durable outbox lease boundary.
type Store interface {
	ClaimOutboxDelivery(context.Context, string, time.Time, time.Duration) (sqlite.OutboxDelivery, error)
	HeartbeatOutboxDelivery(context.Context, string, string, int64, time.Time, time.Duration) error
	CompleteOutboxDelivery(context.Context, string, string, int64, time.Time) error
	FailOutboxDelivery(context.Context, string, string, int64, time.Time, time.Time, bool, string) error
}

// Handler performs one local delivery for a claimed event outbox row.
type Handler interface {
	Handle(context.Context, sqlite.OutboxDelivery) error
}

// Runner claims outbox rows and records bounded terminal outcomes.
type Runner struct {
	Store             Store
	Handler           Handler
	Owner             string
	Lease             time.Duration
	HeartbeatInterval time.Duration
	IdleDelay         time.Duration
	RetryBaseDelay    time.Duration
	RetryMaxDelay     time.Duration
	Now               func() time.Time
	Sleep             func(context.Context, time.Duration) error
}

// ProcessOne handles one eligible outbox delivery.
func (r *Runner) ProcessOne(ctx context.Context) (bool, error) {
	if r.Store == nil || r.Handler == nil || r.Owner == "" {
		return false, errors.New("outbox runner dependencies are required")
	}
	lease := r.Lease
	if lease <= 0 {
		lease = 30 * time.Second
	}
	now := r.now()
	delivery, err := r.Store.ClaimOutboxDelivery(ctx, r.Owner, now, lease)
	if errors.Is(err, sqlite.ErrNoOutboxDelivery) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim outbox delivery: %w", err)
	}
	workCtx, cancelWork := context.WithCancel(ctx)
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	heartbeatErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go r.heartbeat(heartbeatCtx, cancelWork, delivery, lease, heartbeatErr, heartbeatDone)
	err = r.Handler.Handle(workCtx, delivery)
	cancelWork()
	cancelHeartbeat()
	<-heartbeatDone
	select {
	case heartbeatFailure := <-heartbeatErr:
		return true, fmt.Errorf("heartbeat outbox delivery %s: %w", delivery.ID, heartbeatFailure)
	default:
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if err == nil {
		if err := r.Store.CompleteOutboxDelivery(ctx, delivery.ID, r.Owner, delivery.LeaseGeneration, r.now()); err != nil {
			return true, fmt.Errorf("complete outbox delivery: %w", err)
		}
		return true, nil
	}
	retry := !worker.IsPermanent(err) && delivery.Attempt < delivery.MaxAttempts
	if failErr := r.Store.FailOutboxDelivery(ctx, delivery.ID, r.Owner, delivery.LeaseGeneration, r.now(), r.now().Add(r.retryDelay(delivery.Attempt)), retry, err.Error()); failErr != nil {
		return true, fmt.Errorf("fail outbox delivery: %w", failErr)
	}
	return true, nil
}

// Run processes outbox work until cancellation.
func (r *Runner) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
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
		if err := r.sleep(ctx, r.idleDelay()); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("wait for outbox work: %w", err)
		}
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Runner) heartbeat(ctx context.Context, cancel context.CancelFunc, delivery sqlite.OutboxDelivery, lease time.Duration, errorsCh chan<- error, done chan<- struct{}) {
	defer close(done)
	interval := r.HeartbeatInterval
	if interval <= 0 {
		interval = lease / 3
		if interval <= 0 {
			interval = 10 * time.Second
		}
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := r.Store.HeartbeatOutboxDelivery(ctx, delivery.ID, r.Owner, delivery.LeaseGeneration, r.now(), lease); err != nil {
				select {
				case errorsCh <- err:
				default:
				}
				cancel()
				return
			}
			timer.Reset(interval)
		}
	}
}
func (r *Runner) idleDelay() time.Duration {
	if r.IdleDelay > 0 {
		return r.IdleDelay
	}
	return time.Second
}
func (r *Runner) retryDelay(attempt int) time.Duration {
	base, max := r.RetryBaseDelay, r.RetryMaxDelay
	if base <= 0 {
		base = time.Second
	}
	if max <= 0 {
		max = time.Minute
	}
	delay := base
	for i := 1; i < attempt && delay < max; i++ {
		delay *= 2
	}
	if delay > max {
		return max
	}
	return delay
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
