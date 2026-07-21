package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	githubadapter "github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/application/reconcile"
	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/legacy"
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
		return errors.New("usage: reviewctl <config|db|legacy|github> <command> [options]")
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
	case "legacy import":
		return legacyImport(ctx, args[2:], stdout, stderr)
	case "github reconcile":
		return githubReconcile(ctx, args[2:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0]+" "+args[1])
	}
}

func githubReconcile(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("github reconcile", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.DatabasePath, "database", cfg.DatabasePath, "control-plane SQLite database")
	shadow := flags.Bool("shadow", false, "persist factual observations without scheduling or publication")
	connectionID := flags.String("connection-id", "github-local", "stable local GitHub connection identity")
	apiURL := flags.String("api-url", "https://api.github.com", "GitHub API base URL")
	tokenEnvironment := flags.String("token-env", "", "environment variable containing the GitHub token (default GITHUB_TOKEN)")
	tokenFile := flags.String("token-file", "", "file containing the GitHub token")
	timeout := flags.Duration("http-timeout", 30*time.Second, "GitHub HTTP request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*shadow {
		return errors.New("refusing reconciliation without --shadow")
	}
	if *connectionID == "" || *timeout <= 0 {
		return errors.New("connection ID and positive HTTP timeout are required")
	}
	if *tokenFile != "" && *tokenEnvironment != "" {
		return errors.New("--token-file and --token-env are mutually exclusive")
	}

	store, err := storagesqlite.OpenReadOnly(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	status, err := store.SchemaStatus(ctx)
	if err != nil {
		_ = store.Close()
		return err
	}
	if status.Current != status.Latest || status.Current < 3 || status.Pending != 0 {
		_ = store.Close()
		return fmt.Errorf("shadow reconciliation requires a current schema at version 3 or newer: current=%d latest=%d pending=%d", status.Current, status.Latest, status.Pending)
	}
	if err := store.Close(); err != nil {
		return err
	}
	store, err = storagesqlite.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	token, referenceKind, locator, err := githubToken(*tokenEnvironment, *tokenFile)
	if err != nil {
		return err
	}
	client, err := githubadapter.NewClient(*apiURL, token, &http.Client{Timeout: *timeout})
	if err != nil {
		return err
	}
	service, err := reconcile.NewService(client, store)
	if err != nil {
		return err
	}
	report, err := service.Reconcile(ctx, reconcile.Config{
		ConnectionID: *connectionID, APIBaseURL: *apiURL,
		CredentialRefKind: referenceKind, CredentialLocator: locator,
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, report)
}

func githubToken(environmentName, filePath string) (token, referenceKind, locator string, err error) {
	if filePath != "" {
		file, openErr := os.Open(filePath)
		if openErr != nil {
			return "", "", "", fmt.Errorf("open GitHub token file: %w", openErr)
		}
		defer func() { _ = file.Close() }()
		info, statErr := file.Stat()
		if statErr != nil {
			return "", "", "", fmt.Errorf("stat GitHub token file: %w", statErr)
		}
		if !info.Mode().IsRegular() || info.Size() > 64<<10 {
			return "", "", "", errors.New("GitHub token file must be a regular file no larger than 64 KiB")
		}
		contents, readErr := io.ReadAll(io.LimitReader(file, (64<<10)+1))
		if readErr != nil {
			return "", "", "", fmt.Errorf("read GitHub token file: %w", readErr)
		}
		if len(contents) > 64<<10 {
			return "", "", "", errors.New("GitHub token file must be no larger than 64 KiB")
		}
		token = strings.TrimSpace(string(contents))
		absolute, pathErr := filepath.Abs(filePath)
		if pathErr != nil {
			return "", "", "", fmt.Errorf("resolve GitHub token file: %w", pathErr)
		}
		referenceKind, locator = "file", "file:"+absolute
	} else {
		if environmentName == "" {
			environmentName = "GITHUB_TOKEN"
		}
		if environmentName == "" || strings.ContainsRune(environmentName, '=') {
			return "", "", "", errors.New("GitHub token environment variable name is invalid")
		}
		token = strings.TrimSpace(os.Getenv(environmentName))
		referenceKind, locator = "environment", "env:"+environmentName
	}
	if token == "" {
		return "", "", "", errors.New("GitHub token reference resolved to an empty value")
	}
	return token, referenceKind, locator, nil
}

func legacyImport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("legacy import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", "", "manifest-verified legacy backup database")
	manifestPath := flags.String("manifest", "", "backup manifest JSON")
	sourceID := flags.String("source-id", "", "stable legacy source identity")
	displayName := flags.String("source-name", "", "human-readable legacy source name")
	flags.StringVar(&cfg.DatabasePath, "database", cfg.DatabasePath, "control-plane SQLite database")
	apply := flags.Bool("apply", false, "write the validated import plan")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *source == "" {
		return errors.New("--source is required and must name a backup")
	}
	if *sourceID == "" {
		return errors.New("--source-id is required")
	}
	if *manifestPath == "" {
		*manifestPath = *source + ".manifest.json"
	}
	if *displayName == "" {
		*displayName = *sourceID
	}
	if same, err := sameFile(*source, cfg.DatabasePath); err != nil {
		return err
	} else if same {
		return errors.New("legacy source and control-plane target are the same file")
	}

	manifest, err := storagesqlite.VerifyLegacyBackup(ctx, *source, *manifestPath)
	if err != nil {
		return fmt.Errorf("verify import source: %w", err)
	}
	snapshot, err := legacy.ReadSnapshot(ctx, *source)
	if err != nil {
		return fmt.Errorf("read import source: %w", err)
	}
	if _, err := storagesqlite.VerifyLegacyBackup(ctx, *source, *manifestPath); err != nil {
		return fmt.Errorf("reverify import source after snapshot read: %w", err)
	}
	if isLegacy, err := storagesqlite.IsLegacyDatabase(ctx, cfg.DatabasePath); err != nil {
		return err
	} else if isLegacy {
		return errors.New("refusing to import into a legacy database")
	}

	store, err := storagesqlite.OpenReadOnly(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	status, err := store.SchemaStatus(ctx)
	if err != nil {
		_ = store.Close()
		return err
	}
	if status.Current != status.Latest || status.Pending != 0 {
		_ = store.Close()
		return fmt.Errorf("control-plane schema is not current: current=%d latest=%d pending=%d", status.Current, status.Latest, status.Pending)
	}
	if *apply {
		if err := store.Close(); err != nil {
			return err
		}
		store, err = storagesqlite.Open(ctx, cfg.DatabasePath)
		if err != nil {
			return err
		}
	}
	defer func() { _ = store.Close() }()

	input := storagesqlite.LegacyImportInput{
		SourceID:     *sourceID,
		DisplayName:  *displayName,
		SourceReport: manifest.Backup,
		Snapshot:     snapshot,
	}
	var report storagesqlite.LegacyImportReport
	if *apply {
		report, err = store.ImportLegacy(ctx, input)
	} else {
		report, err = store.PlanLegacyImport(ctx, input)
	}
	if err != nil {
		return err
	}
	return writeJSON(stdout, report)
}

func sameFile(leftPath, rightPath string) (bool, error) {
	left, err := os.Stat(leftPath)
	if err != nil {
		return false, fmt.Errorf("stat legacy source: %w", err)
	}
	right, err := os.Stat(rightPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat control-plane target: %w", err)
	}
	return os.SameFile(left, right), nil
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
