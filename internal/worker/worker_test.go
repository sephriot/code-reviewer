package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

func TestRunnerProcessOneCompletes(t *testing.T) {
	t.Parallel()
	store := &fakeStore{job: testJob(1, 3)}
	runner := testRunner(store, HandlerFunc(func(ctx context.Context, job sqlite.Job) error {
		if job.ID != "job-1" {
			t.Fatalf("job ID = %q", job.ID)
		}
		return nil
	}))

	processed, err := runner.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = (%v, %v), want (true, nil)", processed, err)
	}
	if store.completeCalls != 1 || store.failCalls != 0 {
		t.Fatalf("complete=%d fail=%d", store.completeCalls, store.failCalls)
	}
}

func TestRunnerProcessOneRetriesTransientWithCappedBackoff(t *testing.T) {
	t.Parallel()
	store := &fakeStore{job: testJob(3, 5)}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	runner := testRunner(store, HandlerFunc(func(context.Context, sqlite.Job) error {
		return errors.New("network down")
	}))
	runner.Now = func() time.Time { return now }
	runner.RetryBaseDelay = 2 * time.Second
	runner.RetryMaxDelay = 5 * time.Second

	processed, err := runner.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = (%v, %v), want (true, nil)", processed, err)
	}
	if store.failCalls != 1 || !store.failRetry {
		t.Fatalf("fail calls=%d retry=%v", store.failCalls, store.failRetry)
	}
	if got, want := store.retryAt, now.Add(5*time.Second); !got.Equal(want) {
		t.Fatalf("retryAt = %v, want %v", got, want)
	}
	if got, want := store.errorClass, "transient"; got != want {
		t.Fatalf("error class = %q, want %q", got, want)
	}
}

func TestRunnerProcessOneFailsPermanent(t *testing.T) {
	t.Parallel()
	store := &fakeStore{job: testJob(1, 3)}
	runner := testRunner(store, HandlerFunc(func(context.Context, sqlite.Job) error {
		return Permanent(errors.New("invalid input"))
	}))

	processed, err := runner.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("ProcessOne() = (%v, %v), want (true, nil)", processed, err)
	}
	if store.failCalls != 1 || store.failRetry {
		t.Fatalf("fail calls=%d retry=%v", store.failCalls, store.failRetry)
	}
	if got, want := store.errorClass, "permanent"; got != want {
		t.Fatalf("error class = %q, want %q", got, want)
	}
}

func TestRunnerProcessOneCancelsHandlerWhenHeartbeatFails(t *testing.T) {
	t.Parallel()
	heartbeatStarted := make(chan struct{})
	store := &fakeStore{job: testJob(1, 3), heartbeat: func() error {
		close(heartbeatStarted)
		return errors.New("lease lost")
	}}
	handlerStopped := make(chan struct{})
	runner := testRunner(store, HandlerFunc(func(ctx context.Context, _ sqlite.Job) error {
		<-ctx.Done()
		close(handlerStopped)
		return ctx.Err()
	}))
	runner.HeartbeatInterval = time.Millisecond

	processed, err := runner.ProcessOne(context.Background())
	if !processed || err == nil {
		t.Fatalf("ProcessOne() = (%v, %v), want processed heartbeat error", processed, err)
	}
	<-heartbeatStarted
	<-handlerStopped
	if store.completeCalls != 0 || store.failCalls != 0 {
		t.Fatalf("heartbeat loss completed=%d failed=%d", store.completeCalls, store.failCalls)
	}
}

func TestRunnerRunWaitsForWorkUntilCanceled(t *testing.T) {
	t.Parallel()
	store := &fakeStore{claimErr: sqlite.ErrNoJob}
	enteredSleep := make(chan struct{})
	runner := testRunner(store, HandlerFunc(func(context.Context, sqlite.Job) error { return nil }))
	runner.Sleep = func(ctx context.Context, _ time.Duration) error {
		close(enteredSleep)
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	<-enteredSleep
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func testRunner(store Store, handler Handler) *Runner {
	return &Runner{
		Store:             store,
		Handler:           handler,
		Owner:             "worker-test",
		Lease:             time.Second,
		HeartbeatInterval: time.Hour,
		IdleDelay:         time.Millisecond,
		RetryBaseDelay:    time.Second,
		RetryMaxDelay:     time.Minute,
	}
}

func testJob(attempt, maxAttempts int) sqlite.Job {
	return sqlite.Job{
		ID:              "job-1",
		Kind:            "test",
		Attempt:         attempt,
		MaxAttempts:     maxAttempts,
		LeaseOwner:      "worker-test",
		LeaseGeneration: 4,
	}
}

type fakeStore struct {
	mu sync.Mutex

	job       sqlite.Job
	claimErr  error
	heartbeat func() error

	completeCalls int
	failCalls     int
	failRetry     bool
	retryAt       time.Time
	errorClass    string
}

func (s *fakeStore) ClaimJob(context.Context, string, time.Time, time.Duration) (sqlite.Job, error) {
	if s.claimErr != nil {
		return sqlite.Job{}, s.claimErr
	}
	return s.job, nil
}

func (s *fakeStore) HeartbeatJob(context.Context, string, string, int64, time.Time, time.Duration) error {
	if s.heartbeat != nil {
		return s.heartbeat()
	}
	return nil
}

func (s *fakeStore) CompleteJob(context.Context, string, string, int64, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls++
	return nil
}

func (s *fakeStore) FailJob(_ context.Context, _ string, _ string, _ int64, _ time.Time, retryAt time.Time, retry bool, errorClass, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCalls++
	s.failRetry = retry
	s.retryAt = retryAt
	s.errorClass = errorClass
	return nil
}
