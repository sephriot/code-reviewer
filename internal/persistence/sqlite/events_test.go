package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAppendEventWithOutboxCommitsLinkedRows(t *testing.T) {
	ctx := context.Background()
	store := openMigratedEventStore(t, ctx)
	occurredAt := time.Date(2026, 7, 21, 12, 30, 45, 123456000, time.FixedZone("test", 2*60*60))
	availableAt := occurredAt.Add(5 * time.Second)

	result, err := store.AppendEventWithOutbox(ctx, DomainEventInput{
		ID:            "event_requested_1",
		AggregateType: "review_intent",
		AggregateID:   "intent_123",
		EventType:     "review.requested",
		EventVersion:  1,
		Payload:       []byte(`{"intent_id":"intent_123"}`),
		CorrelationID: "correlation_123",
		CausationID:   "command_123",
		OccurredAt:    occurredAt,
	}, []OutboxInput{
		{
			Topic:       "review.requested",
			Payload:     []byte(`{"intent_id":"intent_123"}`),
			AvailableAt: availableAt,
			MaxAttempts: 7,
		},
		{
			Topic:   "audit.recorded",
			Payload: []byte(`{"kind":"review.requested"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EventID == "" || result.Sequence < 1 {
		t.Fatalf("result = %+v, want event identity", result)
	}
	if len(result.OutboxIDs) != 2 || result.OutboxIDs[0] == "" || result.OutboxIDs[1] == "" {
		t.Fatalf("outbox IDs = %v, want two identities", result.OutboxIDs)
	}

	var aggregateType, aggregateID, eventType string
	var version int
	var payload []byte
	var correlationID, causationID sql.NullString
	var occurredAtMicros int64
	err = store.db.QueryRowContext(ctx, `
SELECT aggregate_type, aggregate_id, event_type, event_version, payload_json,
       correlation_id, causation_id, occurred_at_us
FROM domain_events
WHERE id = ?`, result.EventID).Scan(
		&aggregateType,
		&aggregateID,
		&eventType,
		&version,
		&payload,
		&correlationID,
		&causationID,
		&occurredAtMicros,
	)
	if err != nil {
		t.Fatal(err)
	}
	if aggregateType != "review_intent" || aggregateID != "intent_123" || eventType != "review.requested" || version != 1 {
		t.Fatalf("stored event = %s/%s %s v%d", aggregateType, aggregateID, eventType, version)
	}
	if string(payload) != `{"intent_id":"intent_123"}` {
		t.Fatalf("stored event payload = %s", payload)
	}
	if correlationID.String != "correlation_123" || causationID.String != "command_123" {
		t.Fatalf("stored tracing = correlation %q, causation %q", correlationID.String, causationID.String)
	}
	if occurredAtMicros != occurredAt.UTC().UnixMicro() {
		t.Fatalf("occurred_at_us = %d, want %d", occurredAtMicros, occurredAt.UTC().UnixMicro())
	}

	rows, err := store.db.QueryContext(ctx, `
SELECT id, event_id, topic, available_at_us, max_attempts
FROM outbox
ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type outboxRow struct {
		id          string
		eventID     string
		topic       string
		availableAt int64
		maxAttempts int
	}
	var got []outboxRow
	for rows.Next() {
		var row outboxRow
		if err := rows.Scan(&row.id, &row.eventID, &row.topic, &row.availableAt, &row.maxAttempts); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("outbox rows = %v, want two", got)
	}
	for i := range got {
		if got[i].id != result.OutboxIDs[i] || got[i].eventID != result.EventID {
			t.Fatalf("outbox row %d = %+v, result = %+v", i, got[i], result)
		}
	}
	if got[0].topic != "review.requested" || got[0].availableAt != availableAt.UTC().UnixMicro() || got[0].maxAttempts != 7 {
		t.Fatalf("first outbox row = %+v", got[0])
	}
	if got[1].topic != "audit.recorded" || got[1].maxAttempts != 10 {
		t.Fatalf("second outbox row = %+v", got[1])
	}
}

func TestAppendEventWithOutboxRollsBackEventForInvalidOutbox(t *testing.T) {
	ctx := context.Background()
	store := openMigratedEventStore(t, ctx)

	_, err := store.AppendEventWithOutbox(ctx, DomainEventInput{
		ID:            "event_invalid_1",
		AggregateType: "review_intent",
		AggregateID:   "intent_123",
		EventType:     "review.requested",
		EventVersion:  1,
		Payload:       []byte(`{"intent_id":"intent_123"}`),
	}, []OutboxInput{
		{Topic: "review.requested", Payload: []byte(`{"valid":true}`)},
		{Topic: "audit.recorded", Payload: []byte(`{"invalid"`)},
	})
	if err == nil {
		t.Fatal("AppendEventWithOutbox error = nil, want invalid JSON error")
	}

	assertTableCount(t, ctx, store.db, "domain_events", 0)
	assertTableCount(t, ctx, store.db, "outbox", 0)
}

func TestOutboxForeignKeyRejectsUnknownEventAndCascades(t *testing.T) {
	ctx := context.Background()
	store := openMigratedEventStore(t, ctx)

	now := time.Now().UTC().UnixMicro()
	_, err := store.db.ExecContext(ctx, `
INSERT INTO outbox(
    id, event_id, topic, payload_json, state, available_at_us,
    max_attempts, created_at_us, updated_at_us
) VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
		"outbox_orphan",
		"event_missing",
		"review.requested",
		[]byte(`{}`),
		now,
		10,
		now,
		now,
	)
	if err == nil {
		t.Fatal("orphan outbox insert error = nil, want foreign key violation")
	}

	result, err := store.AppendEventWithOutbox(ctx, DomainEventInput{
		ID:            "event_delete_1",
		AggregateType: "review_intent",
		AggregateID:   "intent_123",
		EventType:     "review.requested",
		EventVersion:  1,
		Payload:       []byte(`{}`),
	}, []OutboxInput{{Topic: "review.requested", Payload: []byte(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "DELETE FROM domain_events WHERE id = ?", result.EventID); err != nil {
		t.Fatal(err)
	}
	assertTableCount(t, ctx, store.db, "outbox", 0)
}

func TestAppendEventWithOutboxForJobCommitsWithLiveLease(t *testing.T) {
	ctx := context.Background()
	store := openMigratedEventStore(t, ctx)
	now := time.Now().UTC()
	jobID, err := store.EnqueueJob(ctx, JobInput{
		Kind:        "review",
		Payload:     []byte(`{}`),
		AvailableAt: now.Add(-time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimJob(ctx, "worker-a", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	result, err := store.AppendEventWithOutboxForJob(
		ctx,
		jobID,
		"worker-a",
		job.LeaseGeneration,
		testDomainEvent(),
		testOutbox(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.EventID == "" || len(result.OutboxIDs) != 1 {
		t.Fatalf("result = %+v, want linked event and outbox IDs", result)
	}
	assertTableCount(t, ctx, store.db, "domain_events", 1)
	assertTableCount(t, ctx, store.db, "outbox", 1)
}

func TestAppendEventWithOutboxForJobRejectsLostLeaseWithoutWrites(t *testing.T) {
	tests := []struct {
		name       string
		leaseSetup func(*testing.T, context.Context, *Store) (string, string, int64)
	}{
		{
			name: "wrong owner",
			leaseSetup: func(t *testing.T, ctx context.Context, store *Store) (string, string, int64) {
				t.Helper()
				now := time.Now().UTC()
				jobID := enqueueEventTestJob(t, ctx, store, now)
				job, err := store.ClaimJob(ctx, "worker-a", now, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				return jobID, "worker-b", job.LeaseGeneration
			},
		},
		{
			name: "stale generation",
			leaseSetup: func(t *testing.T, ctx context.Context, store *Store) (string, string, int64) {
				t.Helper()
				now := time.Now().UTC()
				jobID := enqueueEventTestJob(t, ctx, store, now.Add(-2*time.Second))
				first, err := store.ClaimJob(ctx, "worker-a", now.Add(-2*time.Second), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.ClaimJob(ctx, "worker-a", now, time.Hour); err != nil {
					t.Fatal(err)
				}
				return jobID, "worker-a", first.LeaseGeneration
			},
		},
		{
			name: "expired lease",
			leaseSetup: func(t *testing.T, ctx context.Context, store *Store) (string, string, int64) {
				t.Helper()
				now := time.Now().UTC()
				jobID := enqueueEventTestJob(t, ctx, store, now.Add(-2*time.Second))
				job, err := store.ClaimJob(ctx, "worker-a", now.Add(-2*time.Second), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				return jobID, "worker-a", job.LeaseGeneration
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openMigratedEventStore(t, ctx)
			jobID, owner, generation := tt.leaseSetup(t, ctx, store)

			_, err := store.AppendEventWithOutboxForJob(
				ctx,
				jobID,
				owner,
				generation,
				testDomainEvent(),
				testOutbox(),
			)
			if !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("error = %v, want ErrLeaseLost", err)
			}
			assertTableCount(t, ctx, store.db, "domain_events", 0)
			assertTableCount(t, ctx, store.db, "outbox", 0)
		})
	}
}

func TestAppendEventWithOutboxIsIdempotentForCallerEventID(t *testing.T) {
	ctx := context.Background()
	store := openMigratedEventStore(t, ctx)
	event := testDomainEvent()
	event.ID = "event_review_completed_intent_123"
	outbox := testOutbox()

	first, err := store.AppendEventWithOutbox(ctx, event, outbox)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendEventWithOutbox(ctx, event, outbox)
	if err != nil {
		t.Fatal(err)
	}
	if second.EventID != first.EventID || second.Sequence != first.Sequence {
		t.Fatalf("second event = %+v, want %+v", second, first)
	}
	if len(second.OutboxIDs) != 1 || second.OutboxIDs[0] != first.OutboxIDs[0] {
		t.Fatalf("second outbox IDs = %v, want %v", second.OutboxIDs, first.OutboxIDs)
	}
	assertTableCount(t, ctx, store.db, "domain_events", 1)
	assertTableCount(t, ctx, store.db, "outbox", 1)
}

func TestAppendEventWithOutboxRejectsReusedEventIDWithDifferentContent(t *testing.T) {
	ctx := context.Background()
	store := openMigratedEventStore(t, ctx)
	event := testDomainEvent()
	event.ID = "event_review_completed_intent_123"
	first, err := store.AppendEventWithOutbox(ctx, event, testOutbox())
	if err != nil {
		t.Fatal(err)
	}

	conflicting := event
	conflicting.Payload = []byte(`{"intent_id":"another_intent"}`)
	_, err = store.AppendEventWithOutbox(ctx, conflicting, testOutbox())
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("error = %v, want ErrIdempotencyConflict", err)
	}
	assertTableCount(t, ctx, store.db, "domain_events", 1)
	assertTableCount(t, ctx, store.db, "outbox", 1)

	var payload []byte
	if err := store.db.QueryRowContext(ctx, "SELECT payload_json FROM domain_events WHERE id = ?", first.EventID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if string(payload) != string(event.Payload) {
		t.Fatalf("stored payload = %s, want original %s", payload, event.Payload)
	}
}

func TestCommitJobResultCommitsAggregateEventOutboxAndJob(t *testing.T) {
	ctx := context.Background()
	store := openMigratedEventStore(t, ctx)
	now := time.Now().UTC()
	jobID := enqueueEventTestJob(t, ctx, store, now.Add(-time.Second))
	job, err := store.ClaimJob(ctx, "worker-a", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	event := testDomainEvent()
	event.ID = "event_job_result_intent_123"

	result, err := store.CommitJobResult(
		ctx,
		jobID,
		"worker-a",
		job.LeaseGeneration,
		func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
INSERT INTO system_state(key, value, updated_at_us)
VALUES ('aggregate:intent_123', 'completed', ?)`, time.Now().UTC().UnixMicro())
			return err
		},
		event,
		testOutbox(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.EventID != event.ID || len(result.OutboxIDs) != 1 {
		t.Fatalf("result = %+v", result)
	}
	mutationCalled := false
	replayed, err := store.CommitJobResult(
		ctx,
		jobID,
		"worker-a",
		job.LeaseGeneration,
		func(context.Context, *sql.Tx) error {
			mutationCalled = true
			return errors.New("must not run")
		},
		event,
		testOutbox(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if mutationCalled {
		t.Fatal("aggregate mutation ran during idempotent result replay")
	}
	if replayed.EventID != result.EventID || replayed.Sequence != result.Sequence || replayed.OutboxIDs[0] != result.OutboxIDs[0] {
		t.Fatalf("replayed result = %+v, want %+v", replayed, result)
	}

	var aggregateValue string
	if err := store.db.QueryRowContext(ctx, "SELECT value FROM system_state WHERE key = 'aggregate:intent_123'").Scan(&aggregateValue); err != nil {
		t.Fatal(err)
	}
	if aggregateValue != "completed" {
		t.Fatalf("aggregate value = %q, want completed", aggregateValue)
	}
	assertTableCount(t, ctx, store.db, "domain_events", 1)
	assertTableCount(t, ctx, store.db, "outbox", 1)

	var state, resultEventID string
	var leaseOwner sql.NullString
	var leaseExpiresAt sql.NullInt64
	if err := store.db.QueryRowContext(ctx, `
SELECT state, lease_owner, lease_expires_at_us, result_event_id
FROM jobs
WHERE id = ?`, jobID).Scan(&state, &leaseOwner, &leaseExpiresAt, &resultEventID); err != nil {
		t.Fatal(err)
	}
	if state != "succeeded" || leaseOwner.Valid || leaseExpiresAt.Valid {
		t.Fatalf("completed job = state %q owner %+v expiry %+v", state, leaseOwner, leaseExpiresAt)
	}
	if resultEventID != event.ID {
		t.Fatalf("result event ID = %q, want %q", resultEventID, event.ID)
	}
}

func TestAppendEventWithOutboxRequiresCallerEventID(t *testing.T) {
	ctx := context.Background()
	store := openMigratedEventStore(t, ctx)
	event := testDomainEvent()
	event.ID = ""
	if _, err := store.AppendEventWithOutbox(ctx, event, testOutbox()); err == nil {
		t.Fatal("blank event ID was accepted")
	}
	assertTableCount(t, ctx, store.db, "domain_events", 0)
	assertTableCount(t, ctx, store.db, "outbox", 0)
}

func TestCommitJobResultRollsBackAggregateAndJobForMutationFailure(t *testing.T) {
	ctx := context.Background()
	store := openMigratedEventStore(t, ctx)
	jobID, job := claimEventTestJob(t, ctx, store)
	mutationErr := errors.New("aggregate write failed")

	_, err := store.CommitJobResult(
		ctx,
		jobID,
		job.LeaseOwner,
		job.LeaseGeneration,
		func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO system_state(key, value, updated_at_us)
VALUES ('aggregate:intent_123', 'completed', ?)`, time.Now().UTC().UnixMicro()); err != nil {
				return err
			}
			return mutationErr
		},
		testDomainEvent(),
		testOutbox(),
	)
	if !errors.Is(err, mutationErr) {
		t.Fatalf("error = %v, want mutation failure", err)
	}
	assertJobResultRolledBack(t, ctx, store, jobID)
}

func TestCommitJobResultRejectsEventOwnedByAnotherJobBeforeMutation(t *testing.T) {
	ctx := context.Background()
	store := openMigratedEventStore(t, ctx)
	now := time.Now().UTC()
	firstJobID := enqueueEventTestJob(t, ctx, store, now.Add(-time.Second))
	secondJobID := enqueueEventTestJob(t, ctx, store, now.Add(-time.Second))
	first, err := store.ClaimJob(ctx, "worker-a", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != firstJobID && first.ID != secondJobID {
		t.Fatalf("unexpected first job %q", first.ID)
	}
	remainingJobID := secondJobID
	if first.ID == secondJobID {
		remainingJobID = firstJobID
	}
	event := testDomainEvent()
	event.ID = "event_shared_result"
	if _, err := store.CommitJobResult(
		ctx,
		first.ID,
		"worker-a",
		first.LeaseGeneration,
		func(context.Context, *sql.Tx) error { return nil },
		event,
		testOutbox(),
	); err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimJob(ctx, "worker-b", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != remainingJobID {
		t.Fatalf("second job = %q, want %q", second.ID, remainingJobID)
	}
	mutationCalled := false
	_, err = store.CommitJobResult(
		ctx,
		second.ID,
		"worker-b",
		second.LeaseGeneration,
		func(context.Context, *sql.Tx) error {
			mutationCalled = true
			return nil
		},
		event,
		testOutbox(),
	)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("shared event result error = %v, want ErrIdempotencyConflict", err)
	}
	if mutationCalled {
		t.Fatal("aggregate mutation ran for event owned by another job")
	}
	assertTableCount(t, ctx, store.db, "domain_events", 1)
	assertTableCount(t, ctx, store.db, "outbox", 1)
}

func TestCommitJobResultRollsBackAggregateAndJobForEventFailure(t *testing.T) {
	ctx := context.Background()
	store := openMigratedEventStore(t, ctx)
	jobID, job := claimEventTestJob(t, ctx, store)
	invalidEvent := testDomainEvent()
	invalidEvent.Payload = []byte(`{"invalid"`)

	_, err := store.CommitJobResult(
		ctx,
		jobID,
		job.LeaseOwner,
		job.LeaseGeneration,
		func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
INSERT INTO system_state(key, value, updated_at_us)
VALUES ('aggregate:intent_123', 'completed', ?)`, time.Now().UTC().UnixMicro())
			return err
		},
		invalidEvent,
		testOutbox(),
	)
	if err == nil {
		t.Fatal("error = nil, want invalid event failure")
	}
	assertJobResultRolledBack(t, ctx, store, jobID)
}

func claimEventTestJob(t *testing.T, ctx context.Context, store *Store) (string, Job) {
	t.Helper()
	now := time.Now().UTC()
	jobID := enqueueEventTestJob(t, ctx, store, now.Add(-time.Second))
	job, err := store.ClaimJob(ctx, "worker-a", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return jobID, job
}

func assertJobResultRolledBack(t *testing.T, ctx context.Context, store *Store, jobID string) {
	t.Helper()
	assertTableCount(t, ctx, store.db, "domain_events", 0)
	assertTableCount(t, ctx, store.db, "outbox", 0)
	var aggregateCount int
	if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM system_state
WHERE key = 'aggregate:intent_123'`).Scan(&aggregateCount); err != nil {
		t.Fatal(err)
	}
	if aggregateCount != 0 {
		t.Fatalf("aggregate row count = %d, want 0", aggregateCount)
	}
	var state string
	var leaseOwner string
	if err := store.db.QueryRowContext(ctx, `
SELECT state, lease_owner
FROM jobs
WHERE id = ?`, jobID).Scan(&state, &leaseOwner); err != nil {
		t.Fatal(err)
	}
	if state != "running" || leaseOwner != "worker-a" {
		t.Fatalf("job after rollback = state %q owner %q", state, leaseOwner)
	}
}

func enqueueEventTestJob(t *testing.T, ctx context.Context, store *Store, availableAt time.Time) string {
	t.Helper()
	jobID, err := store.EnqueueJob(ctx, JobInput{
		Kind:        "review",
		Payload:     []byte(`{}`),
		AvailableAt: availableAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return jobID
}

func testDomainEvent() DomainEventInput {
	return DomainEventInput{
		ID:            "event_completed_1",
		AggregateType: "review_intent",
		AggregateID:   "intent_123",
		EventType:     "review.completed",
		EventVersion:  1,
		Payload:       []byte(`{"intent_id":"intent_123"}`),
	}
}

func testOutbox() []OutboxInput {
	return []OutboxInput{{
		Topic:   "review.completed",
		Payload: []byte(`{"intent_id":"intent_123"}`),
	}}
}

func openMigratedEventStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ApplyMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

func assertTableCount(t *testing.T, ctx context.Context, db *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&got); err != nil { // #nosec G202 -- table is a test constant.
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s row count = %d, want %d", table, got, want)
	}
}
