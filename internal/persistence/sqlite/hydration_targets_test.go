package sqlite

import (
	"context"
	"errors"
	"testing"
)

func TestFindCanonicalHydrationTargetUsesSelectedObservation(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedMetadataObservation(t, ctx, store.db, "pr-1", "observation-old", projectionHeadSHA, projectionBaseSHA, 10)
	head := "3333333333333333333333333333333333333333"
	seedMetadataObservationWithDigest(t, ctx, store.db, "pr-1", "observation-current", head, projectionBaseSHA, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", 11)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id,
 current_observation_id, freshness, updated_at_us)
VALUES ('pr-1', 'repo-1', 'connection-1', NULL, 'observation-current', 'fresh', 11)`); err != nil {
		t.Fatal(err)
	}

	target, err := store.FindCanonicalHydrationTarget(ctx, "connection-1", "OWNER", "REPO-1", 42)
	if err != nil {
		t.Fatal(err)
	}
	if target.ObservationID != "observation-current" || target.PullRequestID != "pr-1" || target.HeadSHA != head || target.BaseSHA != projectionBaseSHA || target.Number != 42 {
		t.Fatalf("target = %+v", target)
	}
	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func TestFindCanonicalHydrationTargetByPullRequestIDUsesCurrentActiveObservation(t *testing.T) {
	ctx := context.Background()
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
	target, err := store.FindCanonicalHydrationTargetByPullRequestID(ctx, "connection-1", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if target.ObservationID != "observation-1" || target.Owner != "owner" || target.Repository != "repo-1" || target.Number != 42 {
		t.Fatalf("target=%+v", target)
	}
	if _, err := store.FindCanonicalHydrationTargetByPullRequestID(ctx, "connection-2", "pr-1"); !errors.Is(err, ErrCanonicalHydrationTargetNotFound) {
		t.Fatalf("wrong connection error=%v", err)
	}
}

func TestFindCanonicalHydrationTargetRejectsWrongOrInactiveIdentity(t *testing.T) {
	ctx := context.Background()
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
	if _, err := store.FindCanonicalHydrationTarget(ctx, "connection-1", "owner", "repo-1", 43); !errors.Is(err, ErrCanonicalHydrationTargetNotFound) {
		t.Fatalf("wrong number error = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
UPDATE connection_repositories SET access_state = 'inaccessible' WHERE connection_id = 'connection-1' AND repository_id = 'repo-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FindCanonicalHydrationTarget(ctx, "connection-1", "owner", "repo-1", 42); !errors.Is(err, ErrCanonicalHydrationTargetNotFound) {
		t.Fatalf("inactive repository error = %v", err)
	}
}
