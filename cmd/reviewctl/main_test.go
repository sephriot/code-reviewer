package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseMigrateRequiresExplicitApply(t *testing.T) {
	var output bytes.Buffer
	err := run(
		context.Background(),
		[]string{"db", "migrate", "--database", filepath.Join(t.TempDir(), "control-plane.db")},
		&output,
		&output,
	)
	if err == nil || !strings.Contains(err.Error(), "--apply") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestDatabaseStatusDoesNotCreateMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	var output bytes.Buffer
	err := run(context.Background(), []string{"db", "status", "--database", path}, &output, &output)
	if err == nil {
		t.Fatal("status accepted a missing database")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("status created database: %v", statErr)
	}
}

func TestDatabaseMigrateThenStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-plane.db")
	var output bytes.Buffer
	if err := run(context.Background(), []string{"db", "migrate", "--database", path, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"current": 2`) {
		t.Fatalf("migration output = %s", output.String())
	}
	output.Reset()
	if err := run(context.Background(), []string{"db", "status", "--database", path}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"pending": 0`) {
		t.Fatalf("status output = %s", output.String())
	}
}

func TestDatabaseMigrateUsesEnvironmentThenCLIOverride(t *testing.T) {
	environmentPath := filepath.Join(t.TempDir(), "environment.db")
	overridePath := filepath.Join(t.TempDir(), "override.db")
	t.Setenv("REVIEWD_DATABASE_PATH", environmentPath)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"db", "migrate", "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(environmentPath); err != nil {
		t.Fatalf("environment database was not created: %v", err)
	}
	output.Reset()
	if err := run(context.Background(), []string{"db", "migrate", "--database", overridePath, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(overridePath); err != nil {
		t.Fatalf("CLI override database was not created: %v", err)
	}
}

func TestSameFileDetectsSymlinkAlias(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.db")
	if err := os.WriteFile(source, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "alias.db")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}

	same, err := sameFile(alias, source)
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Fatal("sameFile() = false for symlink alias")
	}
}

func TestLegacyImportRequiresExplicitBackupAndSourceID(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"legacy", "import"}, &output, &output); err == nil || !strings.Contains(err.Error(), "--source") {
		t.Fatalf("legacy import without source error = %v", err)
	}
	output.Reset()
	if err := run(context.Background(), []string{"legacy", "import", "--source", "backup.db"}, &output, &output); err == nil || !strings.Contains(err.Error(), "--source-id") {
		t.Fatalf("legacy import without source ID error = %v", err)
	}
}

func TestLegacyImportApplyDoesNotCreateMissingTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "missing.db")
	var output bytes.Buffer
	err := run(context.Background(), []string{
		"legacy", "import", "--source", filepath.Join(t.TempDir(), "missing-backup.db"),
		"--source-id", "legacy-test", "--database", target, "--apply",
	}, &output, &output)
	if err == nil {
		t.Fatal("legacy import accepted missing source")
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("legacy import created target: %v", statErr)
	}
}
