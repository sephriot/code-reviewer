package legacy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestReadSnapshotReadsEveryRowDeterministically(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyReaderFixture(t, path)

	first, err := ReadSnapshot(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadSnapshot() error = %v", err)
	}
	second, err := ReadSnapshot(context.Background(), path)
	if err != nil {
		t.Fatalf("second ReadSnapshot() error = %v", err)
	}

	if first.TotalRows != 8 {
		t.Fatalf("TotalRows = %d, want 8", first.TotalRows)
	}
	if len(first.Rows) != 8 {
		t.Fatalf("len(Rows) = %d, want 8", len(first.Rows))
	}
	wantTables := []string{
		"own_prs",
		"pending_approvals",
		"pr_reviews",
		"pr_reviews",
		"review_request_sync_state",
		"review_requests",
		"review_started_comments",
		"review_started_comments",
	}
	for index, want := range wantTables {
		if first.Rows[index].Table != want {
			t.Errorf("Rows[%d].Table = %q, want %q", index, first.Rows[index].Table, want)
		}
	}
	if first.Rows[2].ID != 2 || first.Rows[3].ID != 9 {
		t.Errorf("pr_reviews row IDs = %d, %d, want 2, 9", first.Rows[2].ID, first.Rows[3].ID)
	}
	for table, want := range map[string]int64{
		"own_prs": 1, "pending_approvals": 1, "pr_reviews": 2,
		"review_request_sync_state": 1, "review_requests": 1,
		"review_started_comments": 2,
	} {
		if got := first.TableCounts[table]; got != want {
			t.Errorf("TableCounts[%q] = %d, want %d", table, got, want)
		}
	}

	if string(first.CanonicalJSON) != string(second.CanonicalJSON) {
		t.Fatal("CanonicalJSON changed between reads")
	}
	if first.RowFormatVersion != 1 {
		t.Fatalf("RowFormatVersion = %d, want 1", first.RowFormatVersion)
	}
	if first.RowsetSHA256 != second.RowsetSHA256 {
		t.Fatalf("RowsetSHA256 changed between reads: %q != %q", first.RowsetSHA256, second.RowsetSHA256)
	}
	digest := sha256.Sum256(first.CanonicalJSON)
	if first.RowsetSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("RowsetSHA256 = %q, want digest of CanonicalJSON", first.RowsetSHA256)
	}
	for _, row := range first.Rows {
		rowDigest := sha256.Sum256(row.RawJSON)
		if row.SHA256 != hex.EncodeToString(rowDigest[:]) {
			t.Errorf("%s/%d SHA256 is not digest of RawJSON", row.Table, row.ID)
		}
	}
}

func TestReadSnapshotExtractsFieldsGroupsRowsAndPreservesTimestamps(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyReaderFixture(t, path)

	snapshot, err := ReadSnapshot(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadSnapshot() error = %v", err)
	}

	row := findRow(t, snapshot.Rows, "pr_reviews", 2)
	if row.Repository != "acme/widgets" || row.PRNumber != 42 || row.Title != "Keep exact history" {
		t.Errorf("extracted PR fields = %#v", row)
	}
	if row.Author != "octo" || row.Status != "active" || row.Action != "approve" {
		t.Errorf("extracted review fields = %#v", row)
	}
	if row.Revision == nil || row.Revision.Kind != RevisionLegacyHeadBase {
		t.Fatalf("Revision = %#v, want kind %q", row.Revision, RevisionLegacyHeadBase)
	}
	if row.Revision.Publishable {
		t.Fatal("legacy revision must never be publishable")
	}
	if row.Revision.HeadSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || row.Revision.BaseSHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Errorf("Revision SHAs = %#v", row.Revision)
	}
	resolved := findRow(t, snapshot.Rows, "review_started_comments", 7)
	if resolved.Revision == nil || resolved.Revision.Kind != RevisionLegacyHeadBase || resolved.Revision.BaseSHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("head-only row was not resolved from group: %#v", resolved.Revision)
	}
	upper := findRow(t, snapshot.Rows, "own_prs", 4)
	if upper.Revision == nil || upper.Revision.HeadSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || upper.Revision.BaseSHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("uppercase SHA pair was not normalized: %#v", upper.Revision)
	}
	if !strings.Contains(string(row.RawJSON), `"reviewed_at":"2024-01-02 03:04:05.123456"`) {
		t.Fatalf("mixed timestamp changed in raw JSON: %s", row.RawJSON)
	}
	request := findRow(t, snapshot.Rows, "review_requests", 5)
	if !strings.Contains(string(request.RawJSON), `"last_seen_at":"2024-01-02T03:04:05.987654+00:00"`) {
		t.Fatalf("RFC timestamp changed in raw JSON: %s", request.RawJSON)
	}

	if len(snapshot.Groups) != 2 {
		t.Fatalf("len(Groups) = %d, want 2", len(snapshot.Groups))
	}
	group := snapshot.Groups[0]
	if group.Repository != "acme/widgets" || group.PRNumber != 42 {
		t.Fatalf("Groups[0] identity = %s#%d", group.Repository, group.PRNumber)
	}
	if len(group.Rows) != 6 {
		t.Fatalf("len(Groups[0].Rows) = %d, want 6 distinct rows", len(group.Rows))
	}
}

func TestReadSnapshotUsesSyntheticRevisionsForMissingOrMalformedSHAs(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyReaderFixture(t, path)

	snapshot, err := ReadSnapshot(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadSnapshot() error = %v", err)
	}

	tests := []struct {
		table string
		id    int64
	}{
		{table: "pr_reviews", id: 9},
		{table: "review_started_comments", id: 1},
	}
	for _, test := range tests {
		row := findRow(t, snapshot.Rows, test.table, test.id)
		if row.Revision == nil || row.Revision.Kind != RevisionLegacySynthetic {
			t.Errorf("%s/%d revision = %#v, want synthetic", test.table, test.id, row.Revision)
			continue
		}
		wantKey := "legacy:" + test.table + ":" + stringID(test.id)
		if row.Revision.Key != wantKey {
			t.Errorf("%s/%d key = %q, want %q", test.table, test.id, row.Revision.Key, wantKey)
		}
		if row.Revision.Publishable {
			t.Errorf("%s/%d is publishable", test.table, test.id)
		}
	}

	var syntheticWarnings int
	for _, warning := range snapshot.Warnings {
		if warning.Code == "synthetic_revision" {
			syntheticWarnings++
		}
	}
	if syntheticWarnings < len(tests) {
		t.Fatalf("synthetic revision warnings = %d, want at least %d", syntheticWarnings, len(tests))
	}
}

func TestReadSnapshotDoesNotResolveAmbiguousBaseSHA(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyReaderFixture(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := db.Exec(`INSERT INTO pr_reviews VALUES (10, 'acme/widgets', 42, 'Conflicting base', 'octo', 'approve', '2024-01-02 03:04:05', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'dddddddddddddddddddddddddddddddddddddddd', 'active')`)
	closeErr := db.Close()
	if execErr != nil {
		t.Fatal(execErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}

	snapshot, err := ReadSnapshot(context.Background(), path)
	if err != nil {
		t.Fatalf("ReadSnapshot() error = %v", err)
	}
	row := findRow(t, snapshot.Rows, "review_started_comments", 7)
	if row.Revision == nil || row.Revision.Kind != RevisionLegacySynthetic {
		t.Fatalf("ambiguous head-only revision = %#v, want synthetic", row.Revision)
	}
}

func TestReadSnapshotRequiresAllKnownTablesAndDoesNotCreateMissingFile(t *testing.T) {
	t.Parallel()

	t.Run("missing table", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "partial.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE pr_reviews (id INTEGER PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}

		_, err = ReadSnapshot(context.Background(), path)
		if !errors.Is(err, ErrInvalidSchema) {
			t.Fatalf("ReadSnapshot() error = %v, want ErrInvalidSchema", err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.db")
		_, err := ReadSnapshot(context.Background(), path)
		if err == nil {
			t.Fatal("ReadSnapshot() error = nil, want error")
		}
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("source file was created or unexpected stat error: %v", statErr)
		}
	})
}

func createLegacyReaderFixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close fixture: %v", err)
		}
	}()

	statements := []string{
		`CREATE TABLE pr_reviews (id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, pr_title TEXT, pr_author TEXT, review_action TEXT, reviewed_at TIMESTAMP, head_sha TEXT, base_sha TEXT, status TEXT)`,
		`CREATE TABLE pending_approvals (id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, pr_title TEXT, pr_author TEXT, pr_url TEXT, review_action TEXT, head_sha TEXT, base_sha TEXT, status TEXT, created_at TIMESTAMP)`,
		`CREATE TABLE own_prs (id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, pr_title TEXT, pr_author TEXT, pr_url TEXT, head_sha TEXT, base_sha TEXT, status TEXT, review_action TEXT, created_at TIMESTAMP)`,
		`CREATE TABLE review_started_comments (id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, head_sha TEXT, created_at TIMESTAMP)`,
		`CREATE TABLE review_requests (id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, pr_title TEXT, pr_author TEXT, pr_url TEXT, head_sha TEXT, base_sha TEXT, last_seen_at TIMESTAMP)`,
		`CREATE TABLE review_request_sync_state (id INTEGER PRIMARY KEY, last_synced_at TIMESTAMP)`,
		`INSERT INTO pr_reviews VALUES (9, 'acme/widgets', 42, 'Older row', 'octo', 'request_changes', '2024-01-02T03:04:05Z', 'deadbeef', 'cccccccccccccccccccccccccccccccccccccccc', 'active')`,
		`INSERT INTO pr_reviews VALUES (2, 'acme/widgets', 42, 'Keep exact history', 'octo', 'approve', '2024-01-02 03:04:05.123456', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'active')`,
		`INSERT INTO pending_approvals VALUES (3, 'acme/widgets', 42, 'Pending', 'octo', 'https://example.test/42', 'approve_with_comment', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 'pending', '2024-01-02 03:04:05')`,
		`INSERT INTO own_prs VALUES (4, 'acme/widgets', 42, 'Mine', 'octo', 'https://example.test/42', 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB', 'reviewed', 'approve', '2024-01-02 03:04:05')`,
		`INSERT INTO review_requests VALUES (5, 'acme/widgets', 42, 'Requested', 'octo', 'https://example.test/42', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', '2024-01-02T03:04:05.987654+00:00')`,
		`INSERT INTO review_started_comments VALUES (7, 'acme/widgets', 42, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', '2024-01-02 03:04:05')`,
		`INSERT INTO review_started_comments VALUES (1, 'zeta/gadget', 8, '', '2024-01-02 03:04:05')`,
		`INSERT INTO review_request_sync_state VALUES (1, '2024-01-02 03:04:05')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("execute fixture statement %q: %v", statement, err)
		}
	}
}

func findRow(t *testing.T, rows []Row, table string, id int64) Row {
	t.Helper()
	for _, row := range rows {
		if row.Table == table && row.ID == id {
			return row
		}
	}
	t.Fatalf("row %s/%d not found", table, id)
	return Row{}
}

func stringID(id int64) string {
	const digits = "0123456789"
	if id == 0 {
		return "0"
	}
	var reversed [20]byte
	index := len(reversed)
	for id > 0 {
		index--
		reversed[index] = digits[id%10]
		id /= 10
	}
	return string(reversed[index:])
}
