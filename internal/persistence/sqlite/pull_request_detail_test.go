package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestPullRequestDetailReturnsOnlyCurrentEvidenceWithoutWrites(t *testing.T) {
	ctx := context.Background()
	store, ids := seedPolicyPublicationChain(t, ctx)
	before := ledgerCounts(t, ctx, store)

	detail, err := store.PullRequestDetail(ctx, PullRequestDetailQuery{ConnectionID: "connection-1", PullRequestID: "pr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if detail.ConnectionID != "connection-1" || detail.RepositoryID != "repo-1" || detail.PullRequestID != "pr-1" ||
		detail.Owner != "owner" || detail.Repository != "repo-1" || detail.Number != 42 ||
		detail.Title != "Metadata only" || detail.State != "open" || detail.CurrentRevisionID != ids.RevisionID ||
		detail.CurrentObservationID != ids.ObservationID || detail.CurrentRevisionIdentityKind != "canonical_diff" ||
		detail.CurrentObservedAt.IsZero() || detail.CurrentReviewRunCount != 1 || detail.CurrentProposalRevisionCount != 1 {
		t.Fatalf("detail = %+v", detail)
	}
	if after := ledgerCounts(t, ctx, store); !reflect.DeepEqual(after, before) {
		t.Fatalf("read-only detail changed ledger: before=%v after=%v", before, after)
	}
}

func TestPullRequestDetailIncludesOnlySafeCurrentRunDiagnostics(t *testing.T) {
	ctx := context.Background()
	store, ids := seedPolicyPublicationChain(t, ctx)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO review_run_events(id, run_id, sequence, event_kind, payload_json, occurred_at_us, created_at_us)
VALUES ('failure-event-1', ?, 99, 'failed_terminal', '{"code":"engine_protocol_invalid","unexpected":"must not reach API"}', 61, 61)`, ids.RunID); err != nil {
		t.Fatal(err)
	}

	detail, err := store.PullRequestDetail(ctx, PullRequestDetailQuery{ConnectionID: "connection-1", PullRequestID: "pr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.RunDiagnostics) != 1 {
		t.Fatalf("diagnostics = %+v", detail.RunDiagnostics)
	}
	diagnostic := detail.RunDiagnostics[0]
	if diagnostic.RunID != ids.RunID || diagnostic.EventKind != ReviewRunEventFailedTerminal || diagnostic.Code != "engine_protocol_invalid" || !diagnostic.OccurredAt.Equal(time.UnixMicro(61).UTC()) {
		t.Fatalf("diagnostic = %+v", diagnostic)
	}
}

func TestPullRequestDetailOmitsDiagnosticsWithoutCurrentCanonicalRevision(t *testing.T) {
	ctx := context.Background()
	store, ids := seedPolicyPublicationChain(t, ctx)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO review_run_events(id, run_id, sequence, event_kind, payload_json, occurred_at_us, created_at_us)
VALUES ('failure-event-2', ?, 99, 'failed_terminal', '{"code":"engine_protocol_invalid"}', 61, 61)`, ids.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	detail, err := store.PullRequestDetail(ctx, PullRequestDetailQuery{ConnectionID: "connection-1", PullRequestID: "pr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.RunDiagnostics) != 0 {
		t.Fatalf("diagnostics without current revision = %+v", detail.RunDiagnostics)
	}
}

func TestPullRequestDetailSupportsMetadataOnlyCurrentObservation(t *testing.T) {
	ctx := context.Background()
	store, _ := seedPolicyPublicationChain(t, ctx)
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	detail, err := store.PullRequestDetail(ctx, PullRequestDetailQuery{ConnectionID: "connection-1", PullRequestID: "pr-1"})
	if err != nil {
		t.Fatal(err)
	}
	if detail.CurrentRevisionID != "" || detail.CurrentRevisionIdentityKind != "" || detail.CurrentReviewRunCount != 0 || detail.CurrentProposalRevisionCount != 0 {
		t.Fatalf("metadata-only detail = %+v", detail)
	}
}

func TestPullRequestDetailRejectsInvalidAndMissingLocalIdentity(t *testing.T) {
	ctx := context.Background()
	store, _ := seedPolicyPublicationChain(t, ctx)
	for _, query := range []PullRequestDetailQuery{
		{}, {ConnectionID: "connection-1"}, {PullRequestID: "pr-1"},
	} {
		if _, err := store.PullRequestDetail(ctx, query); err == nil {
			t.Fatalf("accepted invalid detail query: %+v", query)
		}
	}
	_, err := store.PullRequestDetail(ctx, PullRequestDetailQuery{ConnectionID: "connection-1", PullRequestID: "missing"})
	if !errors.Is(err, ErrPullRequestDetailNotFound) {
		t.Fatalf("missing detail error = %v", err)
	}
}
