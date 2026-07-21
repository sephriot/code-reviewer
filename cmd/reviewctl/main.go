package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/sephriot/code-reviewer/internal/application/hydrate"
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
		return errors.New("usage: reviewctl <config|db|legacy|github|profile|review> <command> [options]")
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
	case "github hydrate":
		return githubHydrate(ctx, args[2:], stdout, stderr)
	case "profile create":
		return profileCreate(ctx, args[2:], stdout, stderr)
	case "review queue":
		return reviewQueue(ctx, args[2:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q", args[0]+" "+args[1])
	}
}

const (
	maxManualInputBytes = 64 << 10
)

func profileCreate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := rejectSecretBearingManualArguments(args); err != nil {
		return err
	}
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("profile create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.DatabasePath, "database", cfg.DatabasePath, "control-plane SQLite database")
	key := flags.String("key", "", "stable profile key")
	version := flags.Int("version", 0, "positive immutable profile version")
	name := flags.String("name", "", "human-readable profile name")
	descriptionFile := flags.String("description-file", "", "optional UTF-8 description file")
	instructionsFile := flags.String("instructions-file", "", "UTF-8 instructions file")
	settingsFile := flags.String("settings-file", "", "optional JSON object settings file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*key) == "" || *version <= 0 || strings.TrimSpace(*name) == "" || *instructionsFile == "" {
		return errors.New("--key, positive --version, --name, and --instructions-file are required")
	}
	instructions, err := readBoundedRegularFile(*instructionsFile, maxManualInputBytes)
	if err != nil {
		return fmt.Errorf("read profile instructions: %w", err)
	}
	description := []byte(nil)
	if *descriptionFile != "" {
		description, err = readBoundedRegularFile(*descriptionFile, maxManualInputBytes)
		if err != nil {
			return fmt.Errorf("read profile description: %w", err)
		}
	}
	settings := []byte(`{}`)
	if *settingsFile != "" {
		settings, err = readBoundedRegularFile(*settingsFile, maxManualInputBytes)
		if err != nil {
			return fmt.Errorf("read profile settings: %w", err)
		}
	}
	if err := rejectSecretBearingJSON(settings); err != nil {
		return fmt.Errorf("profile settings: %w", err)
	}
	store, err := openCurrentControlPlane(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	result, err := store.CreateReviewProfileVersion(ctx, storagesqlite.CreateReviewProfileVersionInput{
		ProfileKey: *key, Version: *version, Name: *name, Description: string(description),
		Instructions: string(instructions), SettingsJSON: settings, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

func reviewQueue(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := rejectSecretBearingManualArguments(args); err != nil {
		return err
	}
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("review queue", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.DatabasePath, "database", cfg.DatabasePath, "control-plane SQLite database")
	connectionID := flags.String("connection-id", "", "local GitHub connection identity")
	owner := flags.String("owner", "", "GitHub repository owner")
	repository := flags.String("repository", "", "GitHub repository name")
	number := flags.Int("number", 0, "positive pull request number")
	profileKey := flags.String("profile-key", "", "immutable review profile key")
	profileVersion := flags.Int("profile-version", 0, "positive immutable review profile version")
	accessMode := flags.String("access-mode", "diff_only", "diff_only, selected_files, or read_only_worktree")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*connectionID) == "" || strings.TrimSpace(*owner) == "" || strings.TrimSpace(*repository) == "" || *number <= 0 || strings.TrimSpace(*profileKey) == "" || *profileVersion <= 0 {
		return errors.New("--connection-id, --owner, --repository, positive --number, --profile-key, and positive --profile-version are required")
	}
	engineConfig := []byte(`{"engine_source":"reviewd_config"}`)
	store, err := openCurrentControlPlane(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	coordinate, err := store.ResolveReviewPullRequest(ctx, *connectionID, *owner, *repository, *number)
	if err != nil {
		return err
	}
	profile, err := store.ResolveReviewProfileVersion(ctx, *profileKey, *profileVersion)
	if err != nil {
		return err
	}
	triggerSHA := manualQueueTriggerSHA256(coordinate, profile, *accessMode)
	queued, err := store.QueueReviewRun(ctx, storagesqlite.PrepareReviewRunInput{
		ConnectionID: coordinate.ConnectionID, PullRequestID: coordinate.PullRequestID,
		ProfileID: profile.ProfileID, ProfileVersionID: profile.ProfileVersionID,
		TriggerKind: "manual", TriggerSHA256: triggerSHA, CorrelationID: "reviewctl-manual",
		EngineKind: "cli", EngineConfigJSON: engineConfig, AccessMode: *accessMode,
		RequestedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, queued)
}

func openCurrentControlPlane(ctx context.Context, path string) (*storagesqlite.Store, error) {
	readOnly, err := storagesqlite.OpenReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	status, statusErr := readOnly.SchemaStatus(ctx)
	closeErr := readOnly.Close()
	if statusErr != nil {
		return nil, statusErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if status.Current != status.Latest || status.Current < 7 || status.Pending != 0 {
		return nil, fmt.Errorf("manual review control requires current schema at version 7 or newer: current=%d latest=%d pending=%d", status.Current, status.Latest, status.Pending)
	}
	return storagesqlite.Open(ctx, path)
}

func readBoundedRegularFile(path string, maximum int64) ([]byte, error) {
	if strings.TrimSpace(path) == "" || maximum <= 0 {
		return nil, errors.New("file path is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maximum {
		return nil, errors.New("file must be regular and within size limit")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > maximum {
		return nil, errors.New("file exceeds size limit")
	}
	return contents, nil
}

func manualQueueTriggerSHA256(coordinate storagesqlite.ReviewPullRequestCoordinate, profile storagesqlite.ReviewProfileVersionCoordinate, accessMode string) string {
	value, _ := json.Marshal(struct {
		FormatVersion    int    `json:"format_version"`
		ConnectionID     string `json:"connection_id"`
		PullRequestID    string `json:"pull_request_id"`
		ProfileID        string `json:"profile_id"`
		ProfileVersionID string `json:"profile_version_id"`
		AccessMode       string `json:"access_mode"`
	}{1, coordinate.ConnectionID, coordinate.PullRequestID, profile.ProfileID, profile.ProfileVersionID, accessMode})
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func rejectSecretBearingManualArguments(args []string) error {
	for _, argument := range args {
		if !strings.HasPrefix(argument, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if secretBearingName(name) {
			return errors.New("manual control arguments cannot carry secrets")
		}
	}
	return nil
}

func rejectSecretBearingJSON(raw []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return errors.New("must be valid JSON")
	}
	if err := rejectSecretBearingJSONValue(value); err != nil {
		return err
	}
	return nil
}

func rejectSecretBearingJSONValue(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if secretBearingName(key) {
				return errors.New("cannot contain secret-bearing key")
			}
			if err := rejectSecretBearingJSONValue(nested); err != nil {
				return err
			}
		}
	case []any:
		for _, nested := range typed {
			if err := rejectSecretBearingJSONValue(nested); err != nil {
				return err
			}
		}
	case string:
		if containsLikelySecret(typed) {
			return errors.New("cannot contain secret value")
		}
	}
	return nil
}

func secretBearingName(value string) bool {
	value = strings.ToLower(strings.ReplaceAll(value, "-", "_"))
	for _, marker := range []string{"token", "password", "secret", "credential", "authorization", "api_key", "apikey", "private_key"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func containsLikelySecret(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "ghp_") || strings.HasPrefix(value, "github_pat_") || strings.HasPrefix(value, "sk-") || strings.Contains(value, "token=") || strings.Contains(value, "password=") || strings.Contains(value, "secret=")
}

func githubHydrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("github hydrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.DatabasePath, "database", cfg.DatabasePath, "control-plane SQLite database")
	shadow := flags.Bool("shadow", false, "attach canonical evidence without scheduling or publication")
	connectionID := flags.String("connection-id", "github-local", "stable local GitHub connection identity")
	owner := flags.String("owner", "", "GitHub repository owner")
	repository := flags.String("repository", "", "GitHub repository name")
	number := flags.Int("number", 0, "GitHub pull request number")
	apiURL := flags.String("api-url", "https://api.github.com", "GitHub API base URL")
	tokenEnvironment := flags.String("token-env", "", "environment variable containing the GitHub token (default GITHUB_TOKEN)")
	tokenFile := flags.String("token-file", "", "file containing the GitHub token")
	timeout := flags.Duration("http-timeout", 30*time.Second, "GitHub HTTP request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*shadow {
		return errors.New("refusing canonical hydration without --shadow")
	}
	if *connectionID == "" || *owner == "" || *repository == "" || strings.Contains(*owner, "/") || strings.Contains(*repository, "/") || *number <= 0 || *timeout <= 0 {
		return errors.New("connection ID, repository coordinates, positive pull request number, and positive HTTP timeout are required")
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
	if status.Current != status.Latest || status.Current < 6 || status.Pending != 0 {
		_ = store.Close()
		return fmt.Errorf("canonical hydration requires a current schema at version 6 or newer: current=%d latest=%d pending=%d", status.Current, status.Latest, status.Pending)
	}
	if err := store.Close(); err != nil {
		return err
	}
	store, err = storagesqlite.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	token, _, _, err := githubToken(*tokenEnvironment, *tokenFile)
	if err != nil {
		return err
	}
	client, err := githubadapter.NewClient(*apiURL, token, &http.Client{Timeout: *timeout})
	if err != nil {
		return err
	}
	service := hydrate.Service{Reader: client, Store: store}
	report, err := service.Hydrate(ctx, hydrate.Request{
		ConnectionID: *connectionID,
		Owner:        *owner,
		Repository:   *repository,
		Number:       *number,
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, report)
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
