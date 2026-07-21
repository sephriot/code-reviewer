package sqlite

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendReviewRunEventRecordsBoundedLifecycleDiagnostic(t *testing.T) {
	ctx := context.Background()
	store, runID := seedPreparedReviewRun(t, ctx)

	result, err := store.AppendReviewRunEvent(ctx, AppendReviewRunEventInput{
		RunID: runID, EventKind: ReviewRunEventFailedRetryable,
		DiagnosticCode: "timeout", RetryAfterSeconds: 30,
		OccurredAt: time.Unix(40, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.EventID == "" || result.Sequence != 3 {
		t.Fatalf("result = %+v", result)
	}

	var kind, payload string
	if err := store.db.QueryRowContext(ctx, `
SELECT event_kind, payload_json FROM review_run_events WHERE id = ?`, result.EventID).Scan(&kind, &payload); err != nil {
		t.Fatal(err)
	}
	if kind != ReviewRunEventFailedRetryable || payload != `{"code":"timeout","retry_after_seconds":30}` {
		t.Fatalf("event = %q %q", kind, payload)
	}
	for _, table := range []string{"jobs", "domain_events", "outbox", "assessments"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func TestAppendReviewRunEventMakesTerminalOutcomesIdempotent(t *testing.T) {
	ctx := context.Background()
	store, runID := seedPreparedReviewRun(t, ctx)
	input := AppendReviewRunEventInput{
		RunID: runID, EventKind: ReviewRunEventFailedTerminal,
		DiagnosticCode: "engine_protocol_invalid", OccurredAt: time.Unix(40, 0).UTC(),
	}
	first, err := store.AppendReviewRunEvent(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input.OccurredAt = input.OccurredAt.Add(time.Hour)
	second, err := store.AppendReviewRunEvent(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || first.EventID != second.EventID || first.Sequence != second.Sequence {
		t.Fatalf("results = %+v / %+v", first, second)
	}

	_, err = store.AppendReviewRunEvent(ctx, AppendReviewRunEventInput{
		RunID: runID, EventKind: ReviewRunEventCanceled,
		DiagnosticCode: "canceled_by_request", OccurredAt: time.Unix(41, 0).UTC(),
	})
	if !errors.Is(err, ErrReviewRunEventConflict) {
		t.Fatalf("terminal conflict = %v", err)
	}
	assertTableCount(t, ctx, store.db, "review_run_events", 3)
}

func TestAppendReviewRunEventRefusesSucceededRunAndUnsafeDiagnostics(t *testing.T) {
	ctx := context.Background()
	store, runID := seedPreparedReviewRun(t, ctx)
	prepared := PrepareReviewRunResult{RunID: runID}
	if _, err := store.RecordAssessment(ctx, RecordAssessmentInput{
		RunID: prepared.RunID, Result: testValidatedAssessmentResult(t), RecordedAt: time.Unix(40, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := store.AppendReviewRunEvent(ctx, AppendReviewRunEventInput{
		RunID: runID, EventKind: ReviewRunEventFailedTerminal,
		DiagnosticCode: "validation_failed", OccurredAt: time.Unix(41, 0).UTC(),
	})
	if !errors.Is(err, ErrReviewRunSucceeded) {
		t.Fatalf("succeeded error = %v", err)
	}

	otherStore, otherRunID := seedPreparedReviewRun(t, ctx)
	_, err = otherStore.AppendReviewRunEvent(ctx, AppendReviewRunEventInput{
		RunID: otherRunID, EventKind: ReviewRunEventFailedRetryable,
		DiagnosticCode: "ghp_not_a_safe_diagnostic_or_raw_engine_output", OccurredAt: time.Unix(40, 0).UTC(),
	})
	if err == nil || !strings.Contains(err.Error(), "diagnostic") {
		t.Fatalf("unsafe diagnostic error = %v", err)
	}
	assertTableCount(t, ctx, otherStore.db, "review_run_events", 2)
}

func TestAppendReviewRunEventSerializesConcurrentTerminalRecord(t *testing.T) {
	ctx := context.Background()
	store, runID := seedPreparedReviewRun(t, ctx)
	input := AppendReviewRunEventInput{
		RunID: runID, EventKind: ReviewRunEventCanceled,
		DiagnosticCode: "canceled_by_shutdown", OccurredAt: time.Unix(40, 0).UTC(),
	}

	const callers = 8
	results := make(chan AppendReviewRunEventResult, callers)
	errs := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := store.AppendReviewRunEvent(ctx, input)
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	group.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	created, eventID := 0, ""
	for result := range results {
		if result.Created {
			created++
		}
		if eventID == "" {
			eventID = result.EventID
		}
		if result.EventID != eventID || result.Sequence != 3 {
			t.Fatalf("result = %+v, expected event=%q sequence=3", result, eventID)
		}
	}
	if created != 1 {
		t.Fatalf("created = %d", created)
	}
	assertTableCount(t, ctx, store.db, "review_run_events", 3)
}

func seedPreparedReviewRun(t *testing.T, ctx context.Context) (*Store, string) {
	t.Helper()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	seedReviewProfileVersion(t, ctx, store, "profile-1", "profile-version-1")
	prepared, err := store.PrepareReviewRun(ctx, testPrepareReviewRunInput())
	if err != nil {
		t.Fatal(err)
	}
	return store, prepared.RunID
}
