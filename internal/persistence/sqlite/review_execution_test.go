package sqlite

import (
	"context"
	"testing"
	"time"
)

func TestLoadCurrentCanonicalReviewTargetReturnsVerifiedSelectedEvidence(t *testing.T) {
	ctx := context.Background()
	store, attached := seedCurrentCanonicalReviewTarget(t, ctx)

	target, err := store.LoadCurrentCanonicalReviewTarget(ctx, "connection-1", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if target.ConnectionID != "connection-1" || target.PullRequestID != "pr-1" ||
		target.ObservationID != "observation-1" || target.RevisionID != attached.RevisionID ||
		target.ManifestID != attached.ManifestID || target.ManifestSHA256 == "" || len(target.ManifestJSON) == 0 {
		t.Fatalf("target = %+v", target)
	}
	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func seedCurrentCanonicalReviewTarget(t *testing.T, ctx context.Context) (*Store, CanonicalRevisionResult) {
	t.Helper()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedMetadataObservation(t, ctx, store.db, "pr-1", "observation-1", projectionHeadSHA, projectionBaseSHA, 10)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id,
 current_observation_id, freshness, updated_at_us)
VALUES ('pr-1', 'repo-1', 'connection-1', NULL, 'observation-1', 'fresh', 10)`); err != nil {
		t.Fatal(err)
	}
	revision := testCanonicalRevision(t)
	attached, err := store.AttachCanonicalRevision(ctx, CanonicalRevisionInput{
		ConnectionID: "connection-1", ObservationID: "observation-1",
		HeadSHA: projectionHeadSHA, BaseSHA: projectionBaseSHA,
		IdentityKey: revision.IdentityKey, ManifestSHA256: revision.ManifestSHA256,
		ManifestJSON: revision.Manifest, EntryCount: 1, AttachedAt: time.Unix(20, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, attached
}
