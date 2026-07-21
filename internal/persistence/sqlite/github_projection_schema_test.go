package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"testing"
)

const (
	projectionHeadSHA = "1111111111111111111111111111111111111111"
	projectionBaseSHA = "2222222222222222222222222222222222222222"
	projectionDigest  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestGitHubProjectionSchemaAcceptsCompleteShadowGeneration(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedCompleteGeneration(t, ctx, store.db, "generation-1", 1)
	seedCanonicalProjection(t, ctx, store.db, "pr-1", "revision-1", "observation-1", "generation-1", 10)

	statements := []string{
		`INSERT INTO reconciliation_generation_items(
		 generation_id, connection_id, repository_id, pull_request_id,
		 observation_id, recorded_at_us)
		 VALUES ('generation-1', 'connection-1', 'repo-1', 'pr-1', 'observation-1', 10)`,
		`INSERT INTO pr_relationships(
		 id, connection_id, repository_id, pull_request_id, relationship_kind,
		 subject_database_id, subject_login, source_kind, started_observation_id, started_generation_id,
		 active_from_us, created_at_us, updated_at_us)
		 VALUES ('relationship-1', 'connection-1', 'repo-1', 'pr-1', 'review_requested',
		 9001, 'reviewer', 'reconciliation', 'observation-1', 'generation-1', 10, 10, 10)`,
		`INSERT INTO reconciliation_checkpoints(
		 connection_id, scope_kind, scope_key, query_partition,
		 last_attempt_generation_id, last_complete_generation_id, updated_at_us)
		 VALUES ('connection-1', 'review_requested_search', 'reviewer', 'all',
		 'generation-1', 'generation-1', 11)`,
		`INSERT INTO pull_request_projection_state(
		 pull_request_id, repository_id, connection_id, current_revision_id,
		 current_observation_id, last_complete_generation_id, freshness, updated_at_us)
		 VALUES ('pr-1', 'repo-1', 'connection-1', 'revision-1',
		 'observation-1', 'generation-1', 'fresh', 12)`,
	}
	for index, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("statement %d failed: %v", index, err)
		}
	}

	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("shadow projection created %d rows in %s", count, table)
		}
	}
	var publicationMode string
	if err := store.db.QueryRowContext(ctx,
		"SELECT value FROM system_state WHERE key = 'publication_mode'").Scan(&publicationMode); err != nil {
		t.Fatal(err)
	}
	if publicationMode != "disabled" {
		t.Fatalf("publication mode = %q, want disabled", publicationMode)
	}
}

func TestMetadataObservationDoesNotInventCanonicalRevision(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_observations(
 id, connection_id, repository_id, pull_request_id, revision_id, head_sha, base_sha,
 source_kind, source_priority, facts_format_version, facts_sha256, title,
 author_login, author_database_id, body_sha256, labels_json, is_draft, base_ref, requested_reviewers_json,
 relationship_set_json, github_state, github_updated_at_us, observed_at_us, created_at_us)
VALUES ('observation-metadata', 'connection-1', 'repo-1', 'pr-1', NULL, ?, ?,
 'direct_refresh', 30, 1, ?, 'Metadata only', 'author', 8001, ?, '[]', 0, 'main', '[]', '[]',
 'open', 10, 10, 10)`, projectionHeadSHA, projectionBaseSHA, projectionDigest, projectionDigest); err != nil {
		t.Fatalf("metadata-only observation was rejected: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id,
 current_observation_id, freshness, updated_at_us)
VALUES ('pr-1', 'repo-1', 'connection-1', NULL,
 'observation-metadata', 'fresh', 10)`); err != nil {
		t.Fatalf("metadata-only projection was rejected: %v", err)
	}
	var revisions int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM revisions").Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if revisions != 0 {
		t.Fatalf("metadata projection invented %d revisions", revisions)
	}
}

func TestConnectionSchemaStoresReferencesNotLiteralSecrets(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)

	tests := []struct {
		name      string
		kind      string
		locator   string
		wantError bool
	}{
		{name: "GitHub CLI", kind: "github_cli", locator: "github-cli"},
		{name: "environment reference", kind: "environment", locator: "env:GITHUB_TOKEN"},
		{name: "keychain reference", kind: "keychain", locator: "keychain:code-reviewer/github"},
		{name: "literal token", kind: "environment", locator: "github_pat_literal", wantError: true},
		{name: "empty environment name", kind: "environment", locator: "env:", wantError: true},
		{name: "mismatched reference", kind: "file", locator: "env:GITHUB_TOKEN", wantError: true},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.db.ExecContext(ctx, `
INSERT INTO connections(
 id, provider, mode, auth_kind, api_base_url, account_login,
 credential_ref_kind, credential_locator, state, permissions_json,
 created_at_us, updated_at_us)
VALUES (?, 'github', 'local_user', 'fine_grained_pat', 'https://api.github.com',
 'reviewer', ?, ?, 'unverified', '{}', 1, 1)`, "connection-test-"+string(rune('a'+index)), test.kind, test.locator)
			if test.wantError && err == nil {
				t.Fatal("unsafe credential locator was accepted")
			}
			if !test.wantError && err != nil {
				t.Fatalf("valid credential reference rejected: %v", err)
			}
		})
	}
}

func TestConnectionSchemaAllowsOnlySecureOrLoopbackAPIURLs(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)

	for index, endpoint := range []string{
		"https://api.github.com",
		"http://127.0.0.1:12345",
		"http://localhost:12345",
		"http://[::1]:12345",
	} {
		if _, err := store.db.ExecContext(ctx, `
INSERT INTO connections(
 id, provider, mode, auth_kind, api_base_url, account_login,
 credential_ref_kind, credential_locator, state, permissions_json,
 created_at_us, updated_at_us)
VALUES (?, 'github', 'local_user', 'github_cli', ?, ?,
 'github_cli', 'github-cli', 'unverified', '{}', 1, 1)`,
			"connection-url-"+string(rune('a'+index)), endpoint, "user"+string(rune('a'+index))); err != nil {
			t.Fatalf("valid endpoint %q rejected: %v", endpoint, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO connections(
 id, provider, mode, auth_kind, api_base_url, account_login,
 credential_ref_kind, credential_locator, state, permissions_json,
 created_at_us, updated_at_us)
VALUES ('connection-insecure', 'github', 'local_user', 'github_cli',
 'http://github.example.com', 'insecure', 'github_cli', 'github-cli',
 'unverified', '{}', 1, 1)`); err == nil {
		t.Fatal("non-loopback plaintext endpoint was accepted")
	}
}

func TestReconciliationGenerationCompletenessGatesProjectionChanges(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO reconciliation_generations(
 id, connection_id, scope_kind, scope_key, query_partition, generation_number,
 mode, state, pages_received, result_count, error_class, started_at_us, finished_at_us)
VALUES ('generation-partial', 'connection-1', 'review_requested_search', 'reviewer',
	'all', 1, 'shadow_read_only', 'partial', 1, 1, 'detail_failed', 1, 2)`); err != nil {
		t.Fatal(err)
	}
	seedCanonicalProjection(t, ctx, store.db, "pr-1", "revision-1", "observation-1", "generation-partial", 10)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO reconciliation_generation_items(
 generation_id, connection_id, repository_id, pull_request_id, observation_id, recorded_at_us)
VALUES ('generation-partial', 'connection-1', 'repo-1', 'pr-1', 'observation-1', 10)`); err != nil {
		t.Fatalf("partial generation could not retain positive membership: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pr_relationships(
 id, connection_id, repository_id, pull_request_id, relationship_kind,
 subject_database_id, subject_login, source_kind, started_observation_id,
 started_generation_id, active_from_us, created_at_us, updated_at_us)
VALUES ('relationship-positive', 'connection-1', 'repo-1', 'pr-1', 'review_requested',
 9001, 'reviewer', 'reconciliation', 'observation-1', 'generation-partial', 10, 10, 10)`); err != nil {
		t.Fatalf("partial generation could not retain positive relationship: %v", err)
	}

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO reconciliation_checkpoints(
 connection_id, scope_kind, scope_key, query_partition,
 last_attempt_generation_id, last_complete_generation_id, updated_at_us)
VALUES ('connection-1', 'review_requested_search', 'reviewer', 'all',
 'generation-partial', 'generation-partial', 11)`); err == nil {
		t.Fatal("running generation became complete checkpoint")
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pr_relationships(
 id, connection_id, repository_id, pull_request_id, relationship_kind,
 subject_database_id, subject_login, source_kind, started_observation_id, active_from_us,
 active_until_us, ended_by_generation_id, created_at_us, updated_at_us)
VALUES ('relationship-ended', 'connection-1', 'repo-1', 'pr-1', 'review_requested',
 9001, 'reviewer', 'direct_refresh', 'observation-1', 5, 10,
 'generation-partial', 5, 10)`); err == nil {
		t.Fatal("incomplete generation ended relationship")
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO reconciliation_generations(
 id, connection_id, scope_kind, scope_key, query_partition, generation_number,
 mode, state, pages_expected, pages_received, result_count, coverage_sha256,
 started_at_us, finished_at_us)
VALUES ('generation-bad-complete', 'connection-1', 'authored_search', 'reviewer',
 'all', 1, 'shadow_read_only', 'complete', 2, 1, 1, ?, 1, 2)`, projectionDigest); err == nil {
		t.Fatal("incomplete page coverage was accepted as complete")
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO reconciliation_generations(
 id, connection_id, scope_kind, scope_key, query_partition, generation_number,
 mode, state, pages_received, result_count, started_at_us)
VALUES ('generation-write', 'connection-1', 'authored_search', 'reviewer',
 'all', 2, 'write_enabled', 'running', 0, 0, 1)`); err == nil {
		t.Fatal("non-shadow reconciliation generation was accepted")
	}
}

func TestProjectionPointersCannotCrossPullRequestsOrMoveBackward(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-2", 43)
	seedCanonicalProjectionDirect(t, ctx, store.db, "pr-1", "revision-1", "observation-new", 20)
	seedCanonicalProjectionDirect(t, ctx, store.db, "pr-2", "revision-2", "observation-other", 20)

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id,
 current_observation_id, freshness, updated_at_us)
VALUES ('pr-1', 'repo-1', 'connection-1', 'revision-2',
 'observation-other', 'fresh', 20)`); err == nil {
		t.Fatal("projection pointers crossed pull requests")
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id,
 current_observation_id, freshness, updated_at_us)
VALUES ('pr-1', 'repo-1', 'connection-1', 'revision-1',
 'observation-new', 'fresh', 20)`); err != nil {
		t.Fatal(err)
	}
	seedObservation(t, ctx, store.db, "pr-1", "revision-1", "observation-old", "direct_refresh", "", 10)
	if _, err := store.db.ExecContext(ctx, `
UPDATE pull_request_projection_state
SET current_observation_id = 'observation-old', updated_at_us = 30
WHERE pull_request_id = 'pr-1'`); err == nil {
		t.Fatal("current observation moved backward")
	}
}

func TestActiveRelationshipIsUniquePerSubject(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedCanonicalProjectionDirect(t, ctx, store.db, "pr-1", "revision-1", "observation-1", 10)
	insert := `INSERT INTO pr_relationships(
 id, connection_id, repository_id, pull_request_id, relationship_kind,
 subject_database_id, subject_login, source_kind, started_observation_id, active_from_us,
 created_at_us, updated_at_us)
VALUES (?, 'connection-1', 'repo-1', 'pr-1', 'review_requested',
 9001, 'reviewer', 'direct_refresh', 'observation-1', 10, 10, 10)`
	if _, err := store.db.ExecContext(ctx, insert, "relationship-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, insert, "relationship-2"); err == nil {
		t.Fatal("duplicate active relationship was accepted")
	}
}

func seedProjectionConnection(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
INSERT INTO connections(
 id, provider, mode, auth_kind, api_base_url, account_login,
	account_database_id,
 credential_ref_kind, credential_locator, state, permissions_json,
 created_at_us, updated_at_us)
VALUES ('connection-1', 'github', 'local_user', 'github_cli',
	'https://api.github.com', 'reviewer', 9001, 'github_cli', 'github-cli',
 'active', '{"pull_requests":"read"}', 1, 1)`); err != nil {
		t.Fatal(err)
	}
}

func seedProjectionPullRequest(t *testing.T, ctx context.Context, db *sql.DB, repositoryID, pullRequestID string, number int) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
INSERT OR IGNORE INTO repositories(id, github_node_id, full_name, owner_login, name, created_at_us, updated_at_us, github_id)
VALUES (?, ?, ?, 'owner', ?, 1, 1, ?)`, repositoryID, "node-"+repositoryID, "owner/"+repositoryID, repositoryID, number+1000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT OR IGNORE INTO connection_repositories(
 connection_id, repository_id, github_repository_id, github_node_id,
 access_state, permissions_json, created_at_us, updated_at_us)
VALUES ('connection-1', ?, ?, ?, 'active', '{"pull":"read"}', 2, 2)`, repositoryID, number+1000, "node-"+repositoryID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO pull_requests(id, repository_id, github_id, number, title, author_login,
 html_url, state, created_at_us, updated_at_us)
VALUES (?, ?, ?, ?, 'Projection', 'author', ?, 'open', 3, 3)`,
		pullRequestID, repositoryID, number+2000, number, "https://github.com/owner/"+repositoryID+"/pull/42"); err != nil {
		t.Fatal(err)
	}
}

func seedCompleteGeneration(t *testing.T, ctx context.Context, db *sql.DB, id string, generation int) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
INSERT INTO reconciliation_generations(
 id, connection_id, scope_kind, scope_key, query_partition, generation_number,
 mode, state, pages_expected, pages_received, provider_total, result_count,
 coverage_sha256, started_at_us, finished_at_us)
VALUES (?, 'connection-1', 'review_requested_search', 'reviewer', 'all', ?,
 'shadow_read_only', 'complete', 1, 1, 1, 1, ?, 4, 5)`, id, generation, projectionDigest); err != nil {
		t.Fatal(err)
	}
}

func seedCanonicalProjection(t *testing.T, ctx context.Context, db *sql.DB, pullRequestID, revisionID, observationID, generationID string, observedAt int64) {
	t.Helper()
	seedRevision(t, ctx, db, pullRequestID, revisionID, observedAt)
	seedObservation(t, ctx, db, pullRequestID, revisionID, observationID, "reconciliation", generationID, observedAt)
}

func seedCanonicalProjectionDirect(t *testing.T, ctx context.Context, db *sql.DB, pullRequestID, revisionID, observationID string, observedAt int64) {
	t.Helper()
	seedRevision(t, ctx, db, pullRequestID, revisionID, observedAt)
	seedObservation(t, ctx, db, pullRequestID, revisionID, observationID, "direct_refresh", "", observedAt)
}

func seedRevision(t *testing.T, ctx context.Context, db *sql.DB, pullRequestID, revisionID string, observedAt int64) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
INSERT INTO revisions(
 id, pull_request_id, identity_kind, identity_key, head_sha, base_sha,
 diff_sha256, is_publishable, observed_at_us, created_at_us)
VALUES (?, ?, 'canonical_diff', ?, ?, ?, ?, 1, ?, ?)`,
		revisionID, pullRequestID, "canonical:"+revisionID, projectionHeadSHA,
		projectionBaseSHA, projectionDigest, observedAt, observedAt); err != nil {
		t.Fatal(err)
	}
}

func seedObservation(t *testing.T, ctx context.Context, db *sql.DB, pullRequestID, revisionID, observationID, sourceKind, generationID string, observedAt int64) {
	t.Helper()
	factsDigest := sha256.Sum256([]byte(observationID))
	var generation any
	if generationID != "" {
		generation = generationID
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO pull_request_observations(
 id, connection_id, repository_id, pull_request_id, revision_id, head_sha, base_sha,
 source_kind, source_generation_id, source_priority, facts_format_version,
 facts_sha256, title, author_login, author_database_id, body_sha256, labels_json, is_draft, base_ref,
 requested_reviewers_json, relationship_set_json, github_state,
 github_updated_at_us, observed_at_us, created_at_us)
VALUES (?, 'connection-1', 'repo-1', ?, ?, ?, ?, ?, ?, ?, 1, ?, 'Projection', 'author', 8001, ?,
 '["bug"]', 0, 'main', '["reviewer"]', '["review_requested"]',
	'open', ?, ?, ?)`, observationID, pullRequestID, revisionID,
		projectionHeadSHA, projectionBaseSHA, sourceKind, generation,
		sourcePriority(sourceKind), stringDigest(factsDigest), projectionDigest,
		observedAt, observedAt, observedAt); err != nil {
		t.Fatal(err)
	}
}

func stringDigest(digest [sha256.Size]byte) string {
	const hexadecimal = "0123456789abcdef"
	encoded := make([]byte, sha256.Size*2)
	for index, value := range digest {
		encoded[index*2] = hexadecimal[value>>4]
		encoded[index*2+1] = hexadecimal[value&0x0f]
	}
	return string(encoded)
}

func sourcePriority(sourceKind string) int {
	switch sourceKind {
	case "reconciliation":
		return 10
	case "webhook":
		return 20
	default:
		return 30
	}
}
