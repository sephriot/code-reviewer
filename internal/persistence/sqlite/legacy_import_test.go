package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/legacy"
)

func TestPlanLegacyImportDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	input := testLegacyImportInput(t)

	report, err := store.PlanLegacyImport(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if report.SourceRows != 3 || report.NewLedgerRows != 3 || report.ExistingLedgerRows != 0 {
		t.Fatalf("plan row counts = %+v", report)
	}
	if report.RepositoriesToCreate != 1 || report.PullRequestsToCreate != 1 || report.RevisionsToCreate != 2 {
		t.Fatalf("plan entity counts = %+v", report)
	}
	if report.Applied {
		t.Fatal("dry plan reported writes")
	}
	assertTableCounts(t, ctx, store, map[string]int64{
		"legacy_sources": 0, "legacy_snapshots": 0, "repositories": 0,
		"pull_requests": 0, "revisions": 0, "migration_ledger": 0,
		"legacy_snapshot_rows": 0,
	})
}

func TestImportLegacyIsIdempotentAndNeverPublishes(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	input := testLegacyImportInput(t)

	first, err := store.ImportLegacy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Applied || first.NewLedgerRows != 3 || first.ExistingLedgerRows != 0 || first.SnapshotState != "complete" {
		t.Fatalf("first import = %+v", first)
	}
	assertTableCounts(t, ctx, store, map[string]int64{
		"legacy_sources": 1, "legacy_snapshots": 1, "repositories": 1,
		"pull_requests": 1, "revisions": 2, "migration_ledger": 3,
		"legacy_snapshot_rows": 3, "jobs": 0, "domain_events": 0, "outbox": 0,
	})
	var publishable int64
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM revisions WHERE is_publishable != 0").Scan(&publishable); err != nil {
		t.Fatal(err)
	}
	if publishable != 0 {
		t.Fatalf("publishable legacy revisions = %d", publishable)
	}
	var state string
	var verified int64
	if err := store.db.QueryRowContext(ctx, `SELECT state, verified_row_count FROM legacy_snapshots WHERE id = ?`, first.SnapshotID).Scan(&state, &verified); err != nil {
		t.Fatal(err)
	}
	if state != "complete" || verified != input.Snapshot.TotalRows {
		t.Fatalf("snapshot state=%q verified=%d", state, verified)
	}

	second, err := store.ImportLegacy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Applied || second.NewLedgerRows != 0 || second.ExistingLedgerRows != 3 {
		t.Fatalf("idempotent import = %+v", second)
	}
	assertTableCounts(t, ctx, store, map[string]int64{
		"legacy_sources": 1, "legacy_snapshots": 1, "repositories": 1,
		"pull_requests": 1, "revisions": 2, "migration_ledger": 3,
		"legacy_snapshot_rows": 3, "jobs": 0, "domain_events": 0, "outbox": 0,
	})
}

func TestLegacyImportChecksumConflictFailsBeforeWrites(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	input := testLegacyImportInput(t)
	if _, err := store.ImportLegacy(ctx, input); err != nil {
		t.Fatal(err)
	}

	changed := input
	changed.Snapshot.Rows = append([]legacy.Row(nil), input.Snapshot.Rows...)
	var changedObject map[string]any
	if err := json.Unmarshal(changed.Snapshot.Rows[0].RawJSON, &changedObject); err != nil {
		t.Fatal(err)
	}
	changedObject["changed"] = true
	changed.Snapshot.Rows[0].RawJSON, _ = json.Marshal(changedObject)
	changed.Snapshot.Rows[0].SHA256 = digestBytes(changed.Snapshot.Rows[0].RawJSON)
	changed.Snapshot.Groups = append([]legacy.PullRequestGroup(nil), input.Snapshot.Groups...)
	changed.Snapshot.Groups[0].Rows = append([]legacy.Row(nil), input.Snapshot.Groups[0].Rows...)
	changed.Snapshot.Groups[0].Rows[0] = changed.Snapshot.Rows[0]
	rebuildTestCanonical(t, &changed.Snapshot)
	changed.SourceReport.LogicalSHA256 = changed.Snapshot.RowsetSHA256
	changed.SourceReport.SHA256 = repeatedHex('d')

	if _, err := store.PlanLegacyImport(ctx, changed); !errors.Is(err, ErrLegacyChecksumConflict) {
		t.Fatalf("plan conflict error = %v", err)
	}
	if _, err := store.ImportLegacy(ctx, changed); !errors.Is(err, ErrLegacyChecksumConflict) {
		t.Fatalf("import conflict error = %v", err)
	}
	assertTableCounts(t, ctx, store, map[string]int64{
		"legacy_sources": 1, "legacy_snapshots": 1, "repositories": 1,
		"pull_requests": 1, "revisions": 2, "migration_ledger": 3,
		"legacy_snapshot_rows": 3,
	})
}

func TestLegacyImportResumesIncompleteSnapshot(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	input := testLegacyImportInput(t)
	first, err := store.ImportLegacy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.ExecContext(ctx, `
UPDATE legacy_snapshots
SET state = 'importing', completed_at_us = NULL, verified_row_count = NULL, coverage_sha256 = NULL
WHERE id = ?`, first.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM legacy_snapshot_rows WHERE snapshot_id = ? AND table_name = 'review_request_sync_state'`, first.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM migration_ledger WHERE source_id = ? AND source_table = 'review_request_sync_state'`, input.SourceID); err != nil {
		t.Fatal(err)
	}

	resumed, err := store.ImportLegacy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Applied || resumed.NewLedgerRows != 1 || resumed.ExistingLedgerRows != 2 || resumed.SnapshotState != "complete" {
		t.Fatalf("resumed import = %+v", resumed)
	}
	assertTableCounts(t, ctx, store, map[string]int64{
		"migration_ledger": 3, "legacy_snapshot_rows": 3, "jobs": 0,
		"domain_events": 0, "outbox": 0,
	})
}

func TestCompletedLegacySnapshotRequiresExactMembership(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	input := testLegacyImportInput(t)
	result, err := store.ImportLegacy(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM legacy_snapshot_rows WHERE snapshot_id = ? AND table_name = 'review_request_sync_state'`, result.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO legacy_snapshots(
  id, source_id, physical_sha256, schema_sha256, logical_sha256, rowset_sha256,
  row_format_version, source_size_bytes, table_count, row_count,
  table_counts_json, state, captured_at_us
) VALUES ('later_snapshot', ?, ?, ?, ?, ?, 1, 1, 1, 1, '{}', 'importing', 1)`,
		input.SourceID, repeatedHex('d'), repeatedHex('e'), repeatedHex('f'), repeatedHex('1')); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO migration_ledger(
  id, source_id, snapshot_id, source_table, source_pk, row_checksum,
  row_format_version, raw_json, warnings_json, imported_at_us
) VALUES ('later_ledger', ?, 'later_snapshot', 'review_request_sync_state', '99', ?, 1, '{}', '[]', 1)`,
		input.SourceID, repeatedHex('2')); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO legacy_snapshot_rows(
  snapshot_id, source_id, table_name, source_pk, row_checksum, ledger_id
) VALUES (?, ?, 'review_request_sync_state', '99', ?, 'later_ledger')`,
		result.SnapshotID, input.SourceID, repeatedHex('2')); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportLegacy(ctx, input); err == nil || !strings.Contains(err.Error(), "invalid coverage") {
		t.Fatalf("damaged completed snapshot error = %v", err)
	}
}

func TestLegacyImportLeaseBlocksConcurrentSourceWriter(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	input := testLegacyImportInput(t)
	lease, err := store.acquireLegacyImportLease(ctx, input.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.releaseLegacyImportLease(input.SourceID, lease)

	if _, err := store.ImportLegacy(ctx, input); !errors.Is(err, ErrLegacyImportInProgress) {
		t.Fatalf("concurrent import error = %v", err)
	}
	assertTableCounts(t, ctx, store, map[string]int64{"migration_ledger": 0, "legacy_snapshots": 0})
}

func TestSyntheticRevisionIdentityIsScopedToSource(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	first := testLegacyImportInput(t)
	if _, err := store.ImportLegacy(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.SourceID = "another-legacy-source"
	second.DisplayName = "another legacy source"
	if _, err := store.ImportLegacy(ctx, second); err != nil {
		t.Fatal(err)
	}
	assertTableCounts(t, ctx, store, map[string]int64{
		"legacy_sources": 2, "migration_ledger": 6, "revisions": 3,
		"jobs": 0, "domain_events": 0, "outbox": 0,
	})
}

func assertTableCounts(t *testing.T, ctx context.Context, store *Store, expected map[string]int64) {
	t.Helper()
	for table, want := range expected {
		var got int64
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if got != want {
			t.Fatalf("%s count = %d, want %d", table, got, want)
		}
	}
}

func testLegacyImportInput(t *testing.T) LegacyImportInput {
	t.Helper()
	pairHead := "1111111111111111111111111111111111111111"
	pairBase := "2222222222222222222222222222222222222222"
	rows := []legacy.Row{
		testLegacyRow("pending_approvals", 1, fmt.Sprintf(`{"id":1,"repository":"owner/repo","pr_number":7,"pr_title":"preferred title","pr_author":"octocat","pr_url":"https://github.com/owner/repo/pull/7","head_sha":%q,"base_sha":%q}`, pairHead, pairBase)),
		testLegacyRow("pr_reviews", 2, `{"id":2,"repository":"owner/repo","pr_number":7}`),
		testLegacyRow("review_request_sync_state", 1, `{"id":1,"last_successful_sync_at":"2026-07-21"}`),
	}
	rows[0].Repository = "owner/repo"
	rows[0].PRNumber = 7
	rows[0].Title = "preferred title"
	rows[0].Author = "octocat"
	rows[0].URL = "https://github.com/owner/repo/pull/7"
	rows[0].HeadSHA = pairHead
	rows[0].BaseSHA = pairBase
	rows[0].Revision = &legacy.RevisionIdentity{
		Kind: legacy.RevisionLegacyHeadBase, Key: "legacy:" + pairHead + ":" + pairBase,
		HeadSHA: pairHead, BaseSHA: pairBase,
	}
	rows[1].Repository = "owner/repo"
	rows[1].PRNumber = 7
	rows[1].Revision = &legacy.RevisionIdentity{
		Kind: legacy.RevisionLegacySynthetic, Key: "legacy:pr_reviews:2",
	}

	counts := map[string]int64{
		"own_prs": 0, "pending_approvals": 1, "pr_reviews": 1,
		"review_request_sync_state": 1, "review_requests": 0, "review_started_comments": 0,
	}
	snapshot := legacy.Snapshot{
		Path: "/archive/reviews.db", RowFormatVersion: 1, Rows: rows,
		Groups:      []legacy.PullRequestGroup{{Repository: "owner/repo", PRNumber: 7, Rows: rows[:2]}},
		TableCounts: counts, TotalRows: 3,
		Warnings: []legacy.Warning{
			{Code: "synthetic_revision", Table: "pr_reviews", ID: 2, Message: "row lacks one unambiguous exact 40-hex commit pair"},
			{Code: "ungrouped_row", Table: "review_request_sync_state", ID: 1, Message: "row has no pull-request identity"},
		},
	}
	rebuildTestCanonical(t, &snapshot)
	return LegacyImportInput{
		SourceID: "legacy-reviews-20260721", DisplayName: "legacy reviews",
		SourceReport: DatabaseReport{
			Path: snapshot.Path, SizeBytes: 4096, SHA256: repeatedHex('b'),
			SchemaSHA256: repeatedHex('a'), LogicalSHA256: repeatedHex('c'),
			Integrity: "ok", TableCounts: counts,
		},
		Snapshot: snapshot,
	}
}

func testLegacyRow(table string, id int64, raw string) legacy.Row {
	value := json.RawMessage(raw)
	return legacy.Row{Table: table, ID: id, RawJSON: value, SHA256: digestBytes(value)}
}

func rebuildTestCanonical(t *testing.T, snapshot *legacy.Snapshot) {
	t.Helper()
	type tablePayload struct {
		Name string            `json:"name"`
		Rows []json.RawMessage `json:"rows"`
	}
	type payload struct {
		RowFormatVersion int            `json:"row_format_version"`
		Tables           []tablePayload `json:"tables"`
	}
	tables := []string{"own_prs", "pending_approvals", "pr_reviews", "review_request_sync_state", "review_requests", "review_started_comments"}
	value := payload{RowFormatVersion: 1, Tables: make([]tablePayload, 0, len(tables))}
	for _, table := range tables {
		entry := tablePayload{Name: table, Rows: make([]json.RawMessage, 0)}
		for _, row := range snapshot.Rows {
			if row.Table == table {
				entry.Rows = append(entry.Rows, row.RawJSON)
			}
		}
		value.Tables = append(value.Tables, entry)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.CanonicalJSON = encoded
	snapshot.RowsetSHA256 = digestBytes(encoded)
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func repeatedHex(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return string(value)
}
