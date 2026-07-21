package main

import (
	"bytes"
	"context"
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
	if !strings.Contains(output.String(), `"current": 1`) {
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
