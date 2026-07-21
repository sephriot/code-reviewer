package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestBackupLegacyCreatesVerifiedSnapshotAndManifest(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "legacy.db")
	createLegacyFixture(t, source)
	destination := filepath.Join(dir, "backups", "legacy.db")

	manifest, err := BackupLegacy(ctx, source, destination)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Backup.Integrity != "ok" {
		t.Fatalf("backup integrity = %q", manifest.Backup.Integrity)
	}
	if manifest.SourceBefore.SHA256 != manifest.SourceAfter.SHA256 {
		t.Fatal("backup changed source database")
	}
	if manifest.SourceBefore.LogicalSHA256 == "" || manifest.SourceBefore.LogicalSHA256 != manifest.Backup.LogicalSHA256 {
		t.Fatal("backup logical digest does not match source")
	}
	if manifest.SourceBefore.TableCounts["pr_reviews"] != 1 || manifest.Backup.TableCounts["pr_reviews"] != 1 {
		t.Fatalf("unexpected counts: source=%v backup=%v", manifest.SourceBefore.TableCounts, manifest.Backup.TableCounts)
	}
	if manifest.ManifestPath != destination+".manifest.json" {
		t.Fatalf("manifest path = %q", manifest.ManifestPath)
	}

	data, err := os.ReadFile(manifest.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted BackupManifest
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Backup.SHA256 != manifest.Backup.SHA256 {
		t.Fatal("persisted manifest does not describe returned backup")
	}

	if _, err := BackupLegacy(ctx, source, destination); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("second backup error = %v, want ErrDestinationExists", err)
	}
}

func TestIsLegacyDatabaseDetectsPartialLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
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
	legacy, err := IsLegacyDatabase(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !legacy {
		t.Fatal("partial legacy schema was not detected")
	}
}

func createLegacyFixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	statements := []string{
		`CREATE TABLE pr_reviews (id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, head_sha TEXT, base_sha TEXT)`,
		`CREATE TABLE pending_approvals (id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, head_sha TEXT, base_sha TEXT)`,
		`CREATE TABLE own_prs (id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, head_sha TEXT, base_sha TEXT)`,
		`CREATE TABLE review_requests (id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, head_sha TEXT, base_sha TEXT)`,
		`CREATE TABLE review_request_sync_state (id INTEGER PRIMARY KEY, last_synced_at TEXT)`,
		`CREATE TABLE review_started_comments (id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, comment_id INTEGER)`,
		`INSERT INTO pr_reviews VALUES (1, 'owner/repo', 7, 'head', 'base')`,
		`INSERT INTO review_request_sync_state VALUES (1, '2026-07-21T10:00:00Z')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("execute fixture statement: %v", err)
		}
	}
}
