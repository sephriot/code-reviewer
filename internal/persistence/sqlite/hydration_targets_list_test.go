package sqlite

import (
	"context"
	"testing"
)

func TestListCanonicalHydrationTargetsSelectsOnlyCurrentActiveMissingCanonicalRevision(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedMetadataObservation(t, ctx, store.db, "pr-1", "observation-needs-hydration", projectionHeadSHA, projectionBaseSHA, 10)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id,
 current_observation_id, freshness, updated_at_us)
VALUES ('pr-1', 'repo-1', 'connection-1', NULL, 'observation-needs-hydration', 'fresh', 10)`); err != nil {
		t.Fatal(err)
	}

	targets, err := store.ListCanonicalHydrationTargets(ctx, "connection-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].ObservationID != "observation-needs-hydration" || targets[0].Number != 42 {
		t.Fatalf("targets = %+v", targets)
	}
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-canonical", 43)
	seedCanonicalRevision(t, ctx, store.db, "pr-canonical", "revision-1", projectionHeadSHA, projectionBaseSHA, 11)
	seedMetadataObservationWithDigestAndRevision(t, ctx, store.db, "pr-canonical", "observation-canonical", projectionHeadSHA, projectionBaseSHA, projectionDigest, "revision-1", 11)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id,
 current_observation_id, freshness, updated_at_us)
VALUES ('pr-canonical', 'repo-1', 'connection-1', 'revision-1', 'observation-canonical', 'fresh', 11)`); err != nil {
		t.Fatal(err)
	}
	targets, err = store.ListCanonicalHydrationTargets(ctx, "connection-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].ObservationID != "observation-needs-hydration" {
		t.Fatalf("canonical target re-enqueued: %+v", targets)
	}
	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}
