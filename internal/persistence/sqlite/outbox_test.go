package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOutboxDeliveryClaimCompleteAndRetryAreFenced(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	now := time.Date(2026, 7, 22, 16, 0, 0, 0, time.UTC)
	event, err := store.AppendEventWithOutbox(ctx, DomainEventInput{ID: "event-outbox-1", AggregateType: "policy_evaluation", AggregateID: "evaluation-1", EventType: "policy.evaluated", EventVersion: 1, Payload: []byte(`{}`), OccurredAt: now}, []OutboxInput{{Topic: "notification.dispatch.v1", Payload: []byte(`{"domain_event_id":"event-outbox-1"}`), AvailableAt: now, MaxAttempts: 2}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimOutboxDelivery(ctx, "runner-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != event.OutboxIDs[0] || claimed.State != "delivering" || claimed.Attempt != 1 || claimed.LeaseGeneration != 1 {
		t.Fatalf("claimed = %+v", claimed)
	}
	if err := store.FailOutboxDelivery(ctx, claimed.ID, "runner-a", claimed.LeaseGeneration, now.Add(time.Second), now.Add(2*time.Second), true, "retry"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimOutboxDelivery(ctx, "runner-b", now.Add(time.Second), time.Minute); !errors.Is(err, ErrNoOutboxDelivery) {
		t.Fatalf("early claim error = %v", err)
	}
	claimed, err = store.ClaimOutboxDelivery(ctx, "runner-b", now.Add(2*time.Second), time.Minute)
	if err != nil || claimed.Attempt != 2 || claimed.LeaseGeneration != 2 {
		t.Fatalf("retry claim=%+v err=%v", claimed, err)
	}
	if err := store.CompleteOutboxDelivery(ctx, claimed.ID, "runner-a", claimed.LeaseGeneration, now.Add(3*time.Second)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale complete error = %v", err)
	}
	if err := store.CompleteOutboxDelivery(ctx, claimed.ID, "runner-b", claimed.LeaseGeneration, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimOutboxDelivery(ctx, "runner-c", now.Add(4*time.Second), time.Minute); !errors.Is(err, ErrNoOutboxDelivery) {
		t.Fatalf("final claim error = %v", err)
	}
}
