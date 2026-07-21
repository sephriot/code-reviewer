package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

func TestCanonicalManifestAttachesImmutableObservationWithoutEffects(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedMetadataObservation(t, ctx, store.db, "pr-1", "observation-1", projectionHeadSHA, projectionBaseSHA, 10)
	seedCanonicalRevisionManifest(t, ctx, store.db, "pr-1", "revision-1", "manifest-1", projectionHeadSHA, projectionBaseSHA, 11)

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO observation_revision_links(
 id, observation_id, pull_request_id, connection_id, revision_id, manifest_id,
 attached_at_us, created_at_us)
VALUES ('link-1', 'observation-1', 'pr-1', 'connection-1', 'revision-1', 'manifest-1', 12, 12)`); err != nil {
		t.Fatalf("attach canonical revision: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id,
 current_observation_id, freshness, updated_at_us)
VALUES ('pr-1', 'repo-1', 'connection-1', 'revision-1',
 'observation-1', 'fresh', 12)`); err != nil {
		t.Fatalf("linked revision could not become current projection: %v", err)
	}

	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("canonical attachment created %d rows in %s", count, table)
		}
	}
}

func TestObservationRevisionLinkIsIdempotentAndAppendOnly(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedMetadataObservation(t, ctx, store.db, "pr-1", "observation-1", projectionHeadSHA, projectionBaseSHA, 10)
	seedCanonicalRevisionManifest(t, ctx, store.db, "pr-1", "revision-1", "manifest-1", projectionHeadSHA, projectionBaseSHA, 11)

	insert := `INSERT OR IGNORE INTO observation_revision_links(
 id, observation_id, pull_request_id, connection_id, revision_id, manifest_id,
 attached_at_us, created_at_us)
VALUES (?, 'observation-1', 'pr-1', 'connection-1', 'revision-1', 'manifest-1', 12, 12)`
	if _, err := store.db.ExecContext(ctx, insert, "link-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, insert, "link-retry"); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM observation_revision_links").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("link count = %d, want 1", count)
	}
	if _, err := store.db.ExecContext(ctx, `
UPDATE observation_revision_links SET attached_at_us = 13 WHERE id = 'link-1'`); err == nil {
		t.Fatal("mutable observation revision link was accepted")
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM observation_revision_links WHERE id = 'link-1'`); err == nil {
		t.Fatal("deleting observation revision link was accepted")
	}
}

func TestObservationRevisionLinkRejectsStaleRevisionMismatch(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedMetadataObservation(t, ctx, store.db, "pr-1", "observation-old", projectionHeadSHA, projectionBaseSHA, 10)
	staleHead := "3333333333333333333333333333333333333333"
	seedCanonicalRevisionManifest(t, ctx, store.db, "pr-1", "revision-new", "manifest-new", staleHead, projectionBaseSHA, 11)

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO observation_revision_links(
 id, observation_id, pull_request_id, connection_id, revision_id, manifest_id,
 attached_at_us, created_at_us)
VALUES ('link-stale', 'observation-old', 'pr-1', 'connection-1', 'revision-new', 'manifest-new', 12, 12)`); err == nil {
		t.Fatal("stale observation linked to mismatched canonical revision")
	}
}

func TestRevisionManifestRejectsWrongCanonicalEnvelope(t *testing.T) {
	for _, test := range []struct {
		name       string
		entryCount int
		manifest   string
	}{
		{
			name:       "wrong head",
			entryCount: 1,
			manifest:   canonicalManifestJSON("3333333333333333333333333333333333333333", projectionBaseSHA),
		},
		{
			name:       "wrong entry count",
			entryCount: 2,
			manifest:   canonicalManifestJSON(projectionHeadSHA, projectionBaseSHA),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store := openMigratedStore(t, ctx)
			seedProjectionConnection(t, ctx, store.db)
			seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
			seedCanonicalRevision(t, ctx, store.db, "pr-1", "revision-1", projectionHeadSHA, projectionBaseSHA, 11)

			if _, err := store.db.ExecContext(ctx, `
INSERT INTO revision_manifests(
 id, revision_id, pull_request_id, manifest_format_version, manifest_sha256,
 entry_count, manifest_json, created_at_us)
VALUES ('manifest-1', 'revision-1', 'pr-1', 1, ?, ?, ?, 11)`, projectionDigest, test.entryCount, test.manifest); err == nil {
				t.Fatal("revision manifest accepted mismatched canonical envelope")
			}
		})
	}
}

func TestObservationRevisionLinkRejectsDifferentEmbeddedRevision(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedCanonicalRevision(t, ctx, store.db, "pr-1", "revision-embedded", projectionHeadSHA, projectionBaseSHA, 10)
	seedMetadataObservationWithDigestAndRevision(t, ctx, store.db, "pr-1", "observation-1", projectionHeadSHA, projectionBaseSHA, projectionDigest, "revision-embedded", 11)
	seedCanonicalRevisionManifestWithDigest(t, ctx, store.db, "pr-1", "revision-linked", "manifest-linked", projectionHeadSHA, projectionBaseSHA, strings.Repeat("d", 64), 12)

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO observation_revision_links(
 id, observation_id, pull_request_id, connection_id, revision_id, manifest_id,
 attached_at_us, created_at_us)
VALUES ('link-1', 'observation-1', 'pr-1', 'connection-1', 'revision-linked', 'manifest-linked', 13, 13)`); err == nil {
		t.Fatal("link accepted a revision different from observation revision")
	}
}

func seedMetadataObservation(t *testing.T, ctx context.Context, db *sql.DB, pullRequestID, observationID, headSHA, baseSHA string, observedAt int64) {
	seedMetadataObservationWithDigest(t, ctx, db, pullRequestID, observationID, headSHA, baseSHA, projectionDigest, observedAt)
}

func seedMetadataObservationWithDigest(t *testing.T, ctx context.Context, db *sql.DB, pullRequestID, observationID, headSHA, baseSHA, digest string, observedAt int64) {
	seedMetadataObservationWithDigestAndRevision(t, ctx, db, pullRequestID, observationID, headSHA, baseSHA, digest, "", observedAt)
}

func seedMetadataObservationWithDigestAndRevision(t *testing.T, ctx context.Context, db *sql.DB, pullRequestID, observationID, headSHA, baseSHA, digest, revisionID string, observedAt int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
INSERT INTO pull_request_observations(
 id, connection_id, repository_id, pull_request_id, revision_id, head_sha, base_sha,
 source_kind, source_priority, facts_format_version, facts_sha256, title,
 author_login, author_database_id, body_sha256, labels_json, is_draft, base_ref,
 requested_reviewers_json, relationship_set_json, github_state,
 github_updated_at_us, observed_at_us, created_at_us)
VALUES (?, 'connection-1', 'repo-1', ?, NULLIF(?, ''), ?, ?,
 'direct_refresh', 30, 1, ?, 'Metadata only', 'author', 8001, ?, '[]', 0, 'main', '[]', '[]',
	 'open', ?, ?, ?)`, observationID, pullRequestID, revisionID, headSHA, baseSHA, digest, digest, observedAt, observedAt, observedAt); err != nil {
		t.Fatal(err)
	}
}

func seedCanonicalRevisionManifest(t *testing.T, ctx context.Context, db *sql.DB, pullRequestID, revisionID, manifestID, headSHA, baseSHA string, createdAt int64) {
	seedCanonicalRevisionManifestWithDigest(t, ctx, db, pullRequestID, revisionID, manifestID, headSHA, baseSHA, projectionDigest, createdAt)
}

func seedCanonicalRevisionManifestWithDigest(t *testing.T, ctx context.Context, db *sql.DB, pullRequestID, revisionID, manifestID, headSHA, baseSHA, digest string, createdAt int64) {
	t.Helper()
	seedCanonicalRevisionWithDigest(t, ctx, db, pullRequestID, revisionID, headSHA, baseSHA, digest, createdAt)
	if _, err := db.ExecContext(ctx, `
INSERT INTO revision_manifests(
 id, revision_id, pull_request_id, manifest_format_version, manifest_sha256,
 entry_count, manifest_json, created_at_us)
VALUES (?, ?, ?, 1, ?, 1, ?, ?)`, manifestID, revisionID, pullRequestID, digest, canonicalManifestJSON(headSHA, baseSHA), createdAt); err != nil {
		t.Fatal(err)
	}
}

func seedCanonicalRevision(t *testing.T, ctx context.Context, db *sql.DB, pullRequestID, revisionID, headSHA, baseSHA string, createdAt int64) {
	seedCanonicalRevisionWithDigest(t, ctx, db, pullRequestID, revisionID, headSHA, baseSHA, projectionDigest, createdAt)
}

func seedCanonicalRevisionWithDigest(t *testing.T, ctx context.Context, db *sql.DB, pullRequestID, revisionID, headSHA, baseSHA, digest string, createdAt int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
INSERT INTO revisions(
 id, pull_request_id, identity_kind, identity_key, head_sha, base_sha,
 diff_sha256, is_publishable, observed_at_us, created_at_us)
VALUES (?, ?, 'canonical_diff', ?, ?, ?, ?, 1, ?, ?)`,
		revisionID, pullRequestID, "canonical:"+revisionID, headSHA, baseSHA, digest, createdAt, createdAt); err != nil {
		t.Fatal(err)
	}
}

func canonicalManifestJSON(headSHA, baseSHA string) string {
	return fmt.Sprintf(`{"version":1,"head_sha":%q,"base_sha":%q,"files":[{"path":"internal/example.go","change_type":"modified","base_blob_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","head_blob_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","base_mode":"100644","head_mode":"100644","binary":false,"patch_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}]}`,
		headSHA, baseSHA)
}
