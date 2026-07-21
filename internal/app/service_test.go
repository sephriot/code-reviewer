package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/config"

	_ "modernc.org/sqlite"
)

func TestNewRequiresCurrentSchemaInCheckMode(t *testing.T) {
	cfg := config.Default()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "control-plane.db")
	cfg.MigrationMode = config.MigrationCheck

	service, err := New(context.Background(), cfg)
	if service != nil {
		_ = service.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "schema is not current") {
		t.Fatalf("New() error = %v", err)
	}
}

func TestNewRefusesLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"pr_reviews", "pending_approvals", "own_prs", "review_requests", "review_request_sync_state", "review_started_comments"} {
		if _, err := db.Exec("CREATE TABLE " + table + " (id INTEGER PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DatabasePath = path
	cfg.MigrationMode = config.MigrationApply
	service, err := New(context.Background(), cfg)
	if service != nil {
		_ = service.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "legacy database") {
		t.Fatalf("New() error = %v", err)
	}
}

func TestNewAppliesMigrationsOnlyWhenConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "control-plane.db")
	cfg.MigrationMode = config.MigrationApply

	service, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	cfg.MigrationMode = config.MigrationCheck
	service, err = New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}
