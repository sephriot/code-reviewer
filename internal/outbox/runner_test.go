package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

func TestRunnerCompletesAndClassifiesFailure(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		handler handlerFunc
		retry   bool
	}{
		{name: "complete", handler: func(context.Context, sqlite.OutboxDelivery) error { return nil }},
		{name: "transient", handler: func(context.Context, sqlite.OutboxDelivery) error { return errors.New("offline") }, retry: true},
		{name: "permanent", handler: func(context.Context, sqlite.OutboxDelivery) error { return worker.Permanent(errors.New("bad payload")) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &fakeStore{delivery: sqlite.OutboxDelivery{ID: "outbox-1", Attempt: 1, MaxAttempts: 3, LeaseGeneration: 4}}
			processed, err := (&Runner{Store: store, Handler: testCase.handler, Owner: "runner", Now: func() time.Time { return time.Unix(1, 0).UTC() }}).ProcessOne(context.Background())
			if err != nil || !processed {
				t.Fatalf("processed=%v err=%v", processed, err)
			}
			if testCase.name == "complete" {
				if store.completed != 1 || store.failed != 0 {
					t.Fatalf("store=%+v", store)
				}
				return
			}
			if store.completed != 0 || store.failed != 1 || store.retry != testCase.retry {
				t.Fatalf("store=%+v", store)
			}
		})
	}
}

type handlerFunc func(context.Context, sqlite.OutboxDelivery) error

func (f handlerFunc) Handle(ctx context.Context, delivery sqlite.OutboxDelivery) error {
	return f(ctx, delivery)
}

type fakeStore struct {
	delivery          sqlite.OutboxDelivery
	completed, failed int
	retry             bool
}

func (s *fakeStore) ClaimOutboxDelivery(context.Context, string, time.Time, time.Duration) (sqlite.OutboxDelivery, error) {
	return s.delivery, nil
}
func (s *fakeStore) HeartbeatOutboxDelivery(context.Context, string, string, int64, time.Time, time.Duration) error {
	return nil
}
func (s *fakeStore) CompleteOutboxDelivery(context.Context, string, string, int64, time.Time) error {
	s.completed++
	return nil
}
func (s *fakeStore) FailOutboxDelivery(_ context.Context, _ string, _ string, _ int64, _ time.Time, _ time.Time, retry bool, _ string) error {
	s.failed++
	s.retry = retry
	return nil
}
