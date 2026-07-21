package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/canonical"
)

func TestAttachCanonicalRevisionIsCurrentIdempotentAndEffectFree(t *testing.T) {
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
	revision := testCanonicalRevision(t)
	input := CanonicalRevisionInput{
		ConnectionID: "connection-1", ObservationID: "observation-1",
		HeadSHA: projectionHeadSHA, BaseSHA: projectionBaseSHA,
		IdentityKey: revision.IdentityKey, ManifestSHA256: revision.ManifestSHA256,
		ManifestJSON: revision.Manifest, EntryCount: 1, AttachedAt: time.Unix(20, 0).UTC(),
	}
	first, err := store.AttachCanonicalRevision(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AttachCanonicalRevision(ctx, input)
	if err != nil {
		t.Fatalf("idempotent attach: %v", err)
	}
	if !first.Created || second.Created || first.RevisionID != second.RevisionID || first.ManifestID != second.ManifestID || first.LinkID != second.LinkID {
		t.Fatalf("attach results = %+v / %+v", first, second)
	}
	var currentRevision string
	if err := store.db.QueryRowContext(ctx, `SELECT current_revision_id FROM pull_request_projection_state WHERE pull_request_id = 'pr-1'`).Scan(&currentRevision); err != nil {
		t.Fatal(err)
	}
	if currentRevision != first.RevisionID {
		t.Fatalf("current revision = %q, want %q", currentRevision, first.RevisionID)
	}
	for _, table := range []string{"revisions", "revision_manifests", "observation_revision_links"} {
		assertTableCount(t, ctx, store.db, table, 1)
	}
	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func TestAttachCanonicalRevisionRejectsStaleObservation(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedMetadataObservation(t, ctx, store.db, "pr-1", "observation-old", projectionHeadSHA, projectionBaseSHA, 10)
	seedMetadataObservationWithDigest(t, ctx, store.db, "pr-1", "observation-current", projectionHeadSHA, projectionBaseSHA, strings.Repeat("d", 64), 11)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id,
 current_observation_id, freshness, updated_at_us)
VALUES ('pr-1', 'repo-1', 'connection-1', NULL, 'observation-current', 'fresh', 11)`); err != nil {
		t.Fatal(err)
	}
	revision := testCanonicalRevision(t)
	_, err := store.AttachCanonicalRevision(ctx, CanonicalRevisionInput{
		ConnectionID: "connection-1", ObservationID: "observation-old", HeadSHA: projectionHeadSHA, BaseSHA: projectionBaseSHA,
		IdentityKey: revision.IdentityKey, ManifestSHA256: revision.ManifestSHA256, ManifestJSON: revision.Manifest,
		EntryCount: 1, AttachedAt: time.Unix(20, 0).UTC(),
	})
	if err == nil || !strings.Contains(err.Error(), "selected current observation") {
		t.Fatalf("stale attach error = %v", err)
	}
	assertTableCount(t, ctx, store.db, "revisions", 0)
}

func TestAttachCanonicalRevisionRejectsManifestDigestMismatch(t *testing.T) {
	revision := testCanonicalRevision(t)
	input := CanonicalRevisionInput{
		ConnectionID: "connection-1", ObservationID: "observation-1", HeadSHA: projectionHeadSHA, BaseSHA: projectionBaseSHA,
		IdentityKey: revision.IdentityKey, ManifestSHA256: revision.ManifestSHA256, ManifestJSON: append(append([]byte(nil), revision.Manifest...), ' '),
		EntryCount: 1, AttachedAt: time.Unix(20, 0).UTC(),
	}
	if _, err := (&Store{}).AttachCanonicalRevision(context.Background(), input); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("digest validation error = %v", err)
	}
}

func testCanonicalRevision(t *testing.T) canonical.Revision {
	t.Helper()
	revision, err := canonical.Build(canonical.Input{
		HeadSHA: projectionHeadSHA, BaseSHA: projectionBaseSHA, Complete: true,
		Files: []canonical.FileChange{{
			Path: "internal/example.go", Status: "modified", BaseBlobSHA: projectionBaseSHA,
			HeadBlobSHA: projectionHeadSHA, BaseMode: "100644", HeadMode: "100644", Patch: []byte("-old\n+new\n"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return revision
}
