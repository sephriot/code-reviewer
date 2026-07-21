package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sephriot/code-reviewer/internal/config"
	storagesqlite "github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return errors.New("usage: reviewctl <config|db|legacy> <command> [options]")
	}
	switch args[0] + " " + args[1] {
	case "config validate":
		return validateConfig(stdout)
	case "db status":
		return databaseStatus(ctx, args[2:], stdout, stderr)
	case "db migrate":
		return databaseMigrate(ctx, args[2:], stdout, stderr)
	case "db backup":
		return databaseBackup(ctx, args[2:], stdout, stderr)
	case "db verify-backup":
		return databaseVerifyBackup(ctx, args[2:], stdout, stderr)
	case "legacy inspect":
		return legacyInspect(ctx, args[2:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0]+" "+args[1])
	}
}

func databaseVerifyBackup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("db verify-backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	backup := flags.String("backup", "", "legacy backup database")
	manifest := flags.String("manifest", "", "backup manifest JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *backup == "" {
		return errors.New("--backup is required")
	}
	if *manifest == "" {
		*manifest = *backup + ".manifest.json"
	}
	verified, err := storagesqlite.VerifyLegacyBackup(ctx, *backup, *manifest)
	if err != nil {
		return err
	}
	return writeJSON(stdout, verified)
}

func validateConfig(stdout io.Writer) error {
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}
	return writeJSON(stdout, cfg)
}

func databaseStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("db status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.DatabasePath, "database", cfg.DatabasePath, "control-plane SQLite database")
	if err := flags.Parse(args); err != nil {
		return err
	}
	legacy, err := storagesqlite.IsLegacyDatabase(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	if legacy {
		return errors.New("legacy database detected; use 'reviewctl legacy inspect' instead")
	}
	store, err := storagesqlite.OpenReadOnly(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	status, err := store.SchemaStatus(ctx)
	if err != nil {
		return err
	}
	return writeJSON(stdout, status)
}

func databaseMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("db migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.DatabasePath, "database", cfg.DatabasePath, "control-plane SQLite database")
	apply := flags.Bool("apply", false, "apply pending migrations")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*apply {
		return errors.New("refusing to change schema without --apply")
	}
	legacy, err := storagesqlite.IsLegacyDatabase(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	if legacy {
		return errors.New("refusing to apply v2 migrations to a legacy database; use a separate target path")
	}
	store, err := storagesqlite.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	applied, err := store.ApplyMigrations(ctx)
	if err != nil {
		return err
	}
	status, err := store.SchemaStatus(ctx)
	if err != nil {
		return err
	}
	return writeJSON(stdout, struct {
		Applied []int                      `json:"applied"`
		Status  storagesqlite.SchemaStatus `json:"status"`
	}{Applied: applied, Status: status})
}

func databaseBackup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("db backup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", filepath.Join("data", "reviews.db"), "legacy SQLite source")
	destination := flags.String("destination", "", "new backup file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *destination == "" {
		return errors.New("--destination is required")
	}
	manifest, err := storagesqlite.BackupLegacy(ctx, *source, *destination)
	if err != nil {
		return err
	}
	return writeJSON(stdout, manifest)
}

func legacyInspect(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("legacy inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", filepath.Join("data", "reviews.db"), "legacy SQLite source")
	if err := flags.Parse(args); err != nil {
		return err
	}
	report, err := storagesqlite.InspectLegacy(ctx, *source)
	if err != nil {
		return err
	}
	return writeJSON(stdout, report)
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
