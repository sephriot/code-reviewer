package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
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
