package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestLegacyImportSchemaAcceptsValidEntityGraph(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)

	statements := []string{
		`INSERT INTO legacy_sources(id, source_kind, display_name, location, created_at_us, updated_at_us)
		 VALUES ('source-1', 'sqlite', 'legacy reviews', '/archive/reviews.db', 1, 1)`,
		`INSERT INTO legacy_snapshots(id, source_id, physical_sha256, schema_sha256, logical_sha256,
		 rowset_sha256, row_format_version,
		 source_size_bytes, table_count, row_count, table_counts_json, captured_at_us)
		 VALUES ('snapshot-1', 'source-1', ?, ?, ?, ?, 1, 4096, 6, 1850, '{"pr_reviews":100}', 2)`,
		`INSERT INTO repositories(id, github_node_id, full_name, owner_login, name, created_at_us, updated_at_us)
		 VALUES ('repo-1', NULL, 'sephriot/code-reviewer', 'sephriot', 'code-reviewer', 3, 3)`,
		`INSERT INTO pull_requests(id, repository_id, github_id, number, title, author_login, html_url,
		 state, created_at_us, updated_at_us)
		 VALUES ('pr-1', 'repo-1', 123, 42, 'Migration', 'author', 'https://github.com/sephriot/code-reviewer/pull/42',
		 'unknown', 4, 4)`,
		`INSERT INTO revisions(id, pull_request_id, identity_kind, identity_key, head_sha, base_sha,
		 diff_sha256, is_publishable, observed_at_us, created_at_us)
		 VALUES ('revision-1', 'pr-1', 'legacy_sha_pair', 'head:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
		 NULL, 0, 5, 5)`,
		`INSERT INTO migration_ledger(id, source_id, snapshot_id, source_table, source_pk, row_checksum,
		 row_format_version,
		 raw_json, warnings_json, repository_id, pull_request_id, revision_id, imported_at_us)
		 VALUES ('ledger-1', 'source-1', 'snapshot-1', 'pr_reviews', '7', ?, 1, '{"id":7}', '[]',
		 'repo-1', 'pr-1', 'revision-1', 6)`,
	}
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for index, statement := range statements {
		var args []any
		switch index {
		case 1:
			args = []any{digest, digest, digest, digest}
		case 5:
			args = []any{digest}
		}
		if _, err := store.db.ExecContext(ctx, statement, args...); err != nil {
			t.Fatalf("statement %d failed: %v", index, err)
		}
	}
}

func TestLegacyRevisionSchemaFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedLegacyPullRequest(t, ctx, store.db)

	tests := []struct {
		name string
		sql  string
	}{
		{
			name: "publishable legacy revision",
			sql: `INSERT INTO revisions(id, pull_request_id, identity_kind, identity_key,
			 is_publishable, observed_at_us, created_at_us)
			 VALUES ('revision-publishable', 'pr-1', 'synthetic_legacy', 'legacy:pr_reviews:1', 1, 1, 1)`,
		},
		{
			name: "publishable legacy sha pair",
			sql: `INSERT INTO revisions(id, pull_request_id, identity_kind, identity_key,
			 head_sha, base_sha, is_publishable, observed_at_us, created_at_us)
			 VALUES ('revision-publishable-pair', 'pr-1', 'legacy_sha_pair', 'legacy:pair',
			 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
			 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 1, 1, 1)`,
		},
		{
			name: "unknown identity kind",
			sql: `INSERT INTO revisions(id, pull_request_id, identity_kind, identity_key,
			 is_publishable, observed_at_us, created_at_us)
			 VALUES ('revision-invalid', 'pr-1', 'guess', 'value', 0, 1, 1)`,
		},
		{
			name: "sha-pair identity without commits",
			sql: `INSERT INTO revisions(id, pull_request_id, identity_kind, identity_key,
			 is_publishable, observed_at_us, created_at_us)
			 VALUES ('revision-no-head', 'pr-1', 'legacy_sha_pair', 'head:missing', 0, 1, 1)`,
		},
		{
			name: "noncanonical sha pair",
			sql: `INSERT INTO revisions(id, pull_request_id, identity_kind, identity_key,
			 head_sha, base_sha, is_publishable, observed_at_us, created_at_us)
			 VALUES ('revision-short-head', 'pr-1', 'legacy_sha_pair', 'head:short',
			 'abc', 'def', 0, 1, 1)`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.db.ExecContext(ctx, test.sql); err == nil {
				t.Fatal("unsafe revision was accepted")
			}
		})
	}
}

func TestCanonicalDiffRevisionMayBePublishable(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedLegacyPullRequest(t, ctx, store.db)
	diff := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO revisions(id, pull_request_id, identity_kind, identity_key, head_sha, base_sha,
 diff_sha256, is_publishable, observed_at_us, created_at_us)
VALUES ('revision-canonical', 'pr-1', 'canonical_diff', 'canonical:one',
 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', ?, 1, 1, 1)`, diff); err != nil {
		t.Fatalf("valid canonical revision was rejected: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO revisions(id, pull_request_id, identity_kind, identity_key, head_sha, base_sha,
 is_publishable, observed_at_us, created_at_us)
VALUES ('revision-no-diff', 'pr-1', 'canonical_diff', 'canonical:missing',
 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 1, 1, 1)`); err == nil {
		t.Fatal("publishable canonical revision without a diff digest was accepted")
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO revisions(id, pull_request_id, identity_kind, identity_key, head_sha, base_sha,
 diff_sha256, is_publishable, observed_at_us, created_at_us)
VALUES ('revision-canonical-duplicate', 'pr-1', 'canonical_diff', 'canonical:different-key',
 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', ?, 0, 2, 2)`, diff); err == nil {
		t.Fatal("duplicate canonical revision tuple was accepted")
	}
}

func TestMigrationLedgerRejectsChangedImmutableSourceRow(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedLegacyPullRequest(t, ctx, store.db)
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO legacy_snapshots(id, source_id, physical_sha256, schema_sha256, logical_sha256,
 rowset_sha256, row_format_version,
 source_size_bytes, table_count, row_count, table_counts_json, captured_at_us)
VALUES ('snapshot-1', 'source-1', ?, ?, ?, ?, 1, 1, 1, 1, '{}', 1)`, digest, digest, digest, digest); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO migration_ledger(id, source_id, snapshot_id, source_table, source_pk,
 row_checksum, row_format_version, raw_json, warnings_json, repository_id, pull_request_id, imported_at_us)
 VALUES (?, 'source-1', 'snapshot-1', 'pr_reviews', '7', ?, 1, '{}', '[]', 'repo-1', 'pr-1', 1)`
	if _, err := store.db.ExecContext(ctx, insert, "ledger-1", digest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, insert, "ledger-duplicate", digest); err == nil {
		t.Fatal("duplicate source row version was accepted")
	}
	changedDigest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := store.db.ExecContext(ctx, insert, "ledger-2", changedDigest); err == nil {
		t.Fatal("changed immutable source row was accepted")
	}
}

func TestLegacySnapshotRowsReuseUnchangedLedgerEntry(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedLegacyPullRequest(t, ctx, store.db)
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	physicalOne := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	physicalTwo := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	for _, snapshot := range []struct {
		id       string
		physical string
		logical  string
	}{
		{id: "snapshot-1", physical: physicalOne, logical: digest},
		{id: "snapshot-2", physical: physicalTwo, logical: physicalTwo},
	} {
		if _, err := store.db.ExecContext(ctx, `
INSERT INTO legacy_snapshots(id, source_id, physical_sha256, schema_sha256, logical_sha256,
 rowset_sha256, row_format_version,
 source_size_bytes, table_count, row_count, table_counts_json, captured_at_us)
VALUES (?, 'source-1', ?, ?, ?, ?, 1, 1, 1, 1, '{"pr_reviews":1}', 1)`,
			snapshot.id, snapshot.physical, digest, snapshot.logical, digest); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO migration_ledger(id, source_id, snapshot_id, source_table, source_pk,
 row_checksum, row_format_version, raw_json, warnings_json, repository_id, pull_request_id, imported_at_us)
VALUES ('ledger-1', 'source-1', 'snapshot-1', 'pr_reviews', '7', ?, 1, '{}', '[]',
 'repo-1', 'pr-1', 1)`, digest); err != nil {
		t.Fatal(err)
	}
	for _, snapshotID := range []string{"snapshot-1", "snapshot-2"} {
		if _, err := store.db.ExecContext(ctx, `
INSERT INTO legacy_snapshot_rows(snapshot_id, source_id, table_name, source_pk, row_checksum, ledger_id)
VALUES (?, 'source-1', 'pr_reviews', '7', ?, 'ledger-1')`, snapshotID, digest); err != nil {
			t.Fatalf("link snapshot %s to unchanged ledger row: %v", snapshotID, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO legacy_snapshot_rows(snapshot_id, source_id, table_name, source_pk, row_checksum, ledger_id)
VALUES ('snapshot-2', 'source-1', 'pr_reviews', '7', ?, 'ledger-1')`, digest); err == nil {
		t.Fatal("duplicate row identity within snapshot was accepted")
	}
}

func TestLegacySnapshotIdentityUsesLogicalDigest(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedLegacyPullRequest(t, ctx, store.db)
	logical := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	physicalOne := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	physicalTwo := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	insert := `INSERT INTO legacy_snapshots(id, source_id, physical_sha256, schema_sha256, logical_sha256,
	 rowset_sha256, row_format_version,
 source_size_bytes, table_count, row_count, table_counts_json, captured_at_us)
	 VALUES (?, 'source-1', ?, ?, ?, ?, 1, 1, 1, 1, '{}', 1)`
	if _, err := store.db.ExecContext(ctx, insert, "snapshot-1", physicalOne, logical, logical, logical); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, insert, "snapshot-2", physicalTwo, logical, logical, logical); err == nil {
		t.Fatal("duplicate logical snapshot with different physical bytes was accepted")
	}
}

func TestLegacySnapshotFinalizationIsExplicitAndComplete(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedLegacyPullRequest(t, ctx, store.db)
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO legacy_snapshots(id, source_id, physical_sha256, schema_sha256, logical_sha256,
 rowset_sha256, row_format_version,
 source_size_bytes, table_count, row_count, table_counts_json, state, captured_at_us)
VALUES ('snapshot-1', 'source-1', ?, ?, ?, ?, 1, 1, 1, 1, '{}', 'importing', 1)`,
		digest, digest, digest, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
UPDATE legacy_snapshots
SET state = 'complete', completed_at_us = 2, verified_row_count = 1, coverage_sha256 = ?
WHERE id = 'snapshot-1'`, digest); err != nil {
		t.Fatalf("finalize complete snapshot: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO legacy_snapshots(id, source_id, physical_sha256, schema_sha256, logical_sha256,
 rowset_sha256, row_format_version,
 source_size_bytes, table_count, row_count, table_counts_json, state, captured_at_us)
VALUES ('snapshot-incomplete', 'source-1', ?, ?, ?, ?, 1, 1, 1, 1, '{}', 'complete', 1)`,
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		digest,
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		digest); err == nil {
		t.Fatal("complete snapshot without verification metadata was accepted")
	}
}

func TestLegacyImportForeignKeysRejectOrphans(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_requests(id, repository_id, number, state, created_at_us, updated_at_us)
VALUES ('orphan', 'missing', 1, 'unknown', 1, 1)`); err == nil {
		t.Fatal("orphan pull request was accepted")
	}
}

func TestLegacyImportMigrationPreservesFoundationRows(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 12 {
		t.Fatalf("migration count = %d, want 12", len(migrations))
	}
	if err := store.ensureMigrationTable(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.applyMigration(ctx, migrations[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO jobs(id, kind, payload_json, state, available_at_us, created_at_us, updated_at_us)
VALUES ('job-before-import-schema', 'test', '{}', 'queued', 1, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	applied, err := store.ApplyMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 11 || applied[0] != 2 || applied[1] != 3 || applied[2] != 4 || applied[3] != 5 || applied[4] != 6 || applied[5] != 7 || applied[6] != 8 || applied[7] != 9 || applied[8] != 10 || applied[9] != 11 || applied[10] != 12 {
		t.Fatalf("forward migration result = %v, want [2 3 4 5 6 7 8 9 10 11 12]", applied)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM jobs WHERE id = 'job-before-import-schema'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("foundation row was not preserved")
	}
}

func openMigratedStore(t *testing.T, ctx context.Context) *Store {
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

func seedLegacyPullRequest(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
INSERT INTO legacy_sources(id, source_kind, display_name, location, created_at_us, updated_at_us)
VALUES ('source-1', 'sqlite', 'legacy reviews', '/archive/reviews.db', 1, 1);
INSERT INTO repositories(id, full_name, owner_login, name, created_at_us, updated_at_us)
VALUES ('repo-1', 'sephriot/code-reviewer', 'sephriot', 'code-reviewer', 1, 1);
INSERT INTO pull_requests(id, repository_id, number, state, created_at_us, updated_at_us)
VALUES ('pr-1', 'repo-1', 42, 'unknown', 1, 1)`); err != nil {
		t.Fatal(err)
	}
}
