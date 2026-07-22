// Package config defines the bootstrap configuration for reviewd.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvDatabasePath overrides the control-plane SQLite database path.
	EnvDatabasePath = "REVIEWD_DATABASE_PATH"
	// EnvListenAddress overrides the loopback control API listener.
	EnvListenAddress = "REVIEWD_LISTEN_ADDRESS"
	// EnvMigrationMode chooses whether startup checks or applies migrations.
	EnvMigrationMode = "REVIEWD_MIGRATION_MODE"
	// EnvPublicationMode controls external publication.
	EnvPublicationMode = "REVIEWD_PUBLICATION_MODE"
	// EnvShadowReconcileEnabled enables read-only GitHub reconciliation.
	EnvShadowReconcileEnabled = "REVIEWD_SHADOW_RECONCILE_ENABLED"
	// EnvGitHubConnectionID identifies the locally configured GitHub connection.
	EnvGitHubConnectionID = "REVIEWD_GITHUB_CONNECTION_ID"
	// EnvGitHubAPIBaseURL overrides the GitHub API endpoint for shadow reads.
	EnvGitHubAPIBaseURL = "REVIEWD_GITHUB_API_BASE_URL"
	// EnvGitHubTokenEnvironment names, but never contains, the token environment variable.
	EnvGitHubTokenEnvironment = "REVIEWD_GITHUB_TOKEN_ENVIRONMENT"
	// EnvShadowReconcileInterval controls how often shadow reconciliation is enqueued.
	EnvShadowReconcileInterval = "REVIEWD_SHADOW_RECONCILE_INTERVAL"
	// EnvReviewExecutionEnabled enables local, read-only CLI review execution.
	EnvReviewExecutionEnabled = "REVIEWD_REVIEW_EXECUTION_ENABLED"
	// EnvReviewEngineArgv supplies the trusted review engine command as a JSON argv array.
	EnvReviewEngineArgv = "REVIEWD_REVIEW_ENGINE_ARGV"
)

// MigrationMode controls how reviewd treats pending schema migrations at startup.
type MigrationMode string

const (
	// MigrationCheck reports pending migrations without applying them.
	MigrationCheck MigrationMode = "check"
	// MigrationApply applies pending migrations before serving traffic.
	MigrationApply MigrationMode = "apply"
)

// PublicationMode controls whether authorized effects may reach GitHub.
type PublicationMode string

const (
	// PublicationDisabled persists workflow state without dispatching GitHub mutations.
	PublicationDisabled PublicationMode = "disabled"
	// PublicationSimulated records local simulated publication attempts without
	// granting a GitHub write capability.
	PublicationSimulated PublicationMode = "simulated"
)

// Config contains startup-only control-plane settings.
type Config struct {
	DatabasePath         string                     `json:"database_path"`
	ListenAddress        string                     `json:"listen_address"`
	MigrationMode        MigrationMode              `json:"migration_mode"`
	PublicationMode      PublicationMode            `json:"publication_mode"`
	ShadowReconciliation ShadowReconciliationConfig `json:"shadow_reconciliation"`
	ReviewExecution      ReviewExecutionConfig      `json:"review_execution"`
}

// ShadowReconciliationConfig configures opt-in, GET-only GitHub observation.
// TokenEnvironment is a reference to process environment, never a secret value.
type ShadowReconciliationConfig struct {
	Enabled          bool          `json:"enabled"`
	ConnectionID     string        `json:"connection_id"`
	APIBaseURL       string        `json:"api_base_url"`
	TokenEnvironment string        `json:"token_environment"`
	Interval         time.Duration `json:"interval"`
}

// ReviewExecutionConfig configures a local review engine. EngineArgv is
// executed directly and never interpreted by a shell. Its GitHub read access
// always reuses the configured shadow-reconciliation connection.
type ReviewExecutionConfig struct {
	Enabled    bool     `json:"enabled"`
	EngineArgv []string `json:"engine_argv"`
}

// Default returns the safe local bootstrap configuration.
func Default() Config {
	return Config{
		DatabasePath:    filepath.Join("data", "control-plane.db"),
		ListenAddress:   "127.0.0.1:8080",
		MigrationMode:   MigrationCheck,
		PublicationMode: PublicationDisabled,
		ShadowReconciliation: ShadowReconciliationConfig{
			APIBaseURL: "https://api.github.com",
			Interval:   5 * time.Minute,
		},
	}
}

// LoadEnv reads bootstrap overrides from the process environment.
func LoadEnv() (Config, error) {
	return Load(os.LookupEnv)
}

// Load reads bootstrap overrides through lookup and validates every supplied value.
func Load(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("environment lookup is required")
	}

	cfg := Default()
	if value, ok := lookup(EnvDatabasePath); ok {
		cfg.DatabasePath = strings.TrimSpace(value)
		if err := validateDatabasePath(cfg.DatabasePath); err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvDatabasePath, err)
		}
	}
	if value, ok := lookup(EnvListenAddress); ok {
		cfg.ListenAddress = strings.TrimSpace(value)
		if err := validateListenAddress(cfg.ListenAddress); err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvListenAddress, err)
		}
	}
	if value, ok := lookup(EnvMigrationMode); ok {
		cfg.MigrationMode = MigrationMode(strings.TrimSpace(value))
		if err := validateMigrationMode(cfg.MigrationMode); err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvMigrationMode, err)
		}
	}
	if value, ok := lookup(EnvPublicationMode); ok {
		cfg.PublicationMode = PublicationMode(strings.TrimSpace(value))
		if err := validatePublicationMode(cfg.PublicationMode); err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvPublicationMode, err)
		}
	}
	if value, ok := lookup(EnvShadowReconcileEnabled); ok {
		enabled, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("%s: must be true or false", EnvShadowReconcileEnabled)
		}
		cfg.ShadowReconciliation.Enabled = enabled
	}
	if value, ok := lookup(EnvGitHubConnectionID); ok {
		cfg.ShadowReconciliation.ConnectionID = strings.TrimSpace(value)
	}
	if value, ok := lookup(EnvGitHubAPIBaseURL); ok {
		cfg.ShadowReconciliation.APIBaseURL = strings.TrimSpace(value)
	}
	if value, ok := lookup(EnvGitHubTokenEnvironment); ok {
		cfg.ShadowReconciliation.TokenEnvironment = strings.TrimSpace(value)
	}
	if value, ok := lookup(EnvShadowReconcileInterval); ok {
		interval, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvShadowReconcileInterval, err)
		}
		cfg.ShadowReconciliation.Interval = interval
	}
	if value, ok := lookup(EnvReviewExecutionEnabled); ok {
		enabled, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return Config{}, fmt.Errorf("%s: must be true or false", EnvReviewExecutionEnabled)
		}
		cfg.ReviewExecution.Enabled = enabled
	}
	if value, ok := lookup(EnvReviewEngineArgv); ok {
		argv, err := parseEngineArgv(value)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", EnvReviewEngineArgv, err)
		}
		cfg.ReviewExecution.EngineArgv = argv
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate rejects unsafe or unsupported startup configuration.
func (cfg Config) Validate() error {
	if err := validateDatabasePath(cfg.DatabasePath); err != nil {
		return err
	}
	if err := validateListenAddress(cfg.ListenAddress); err != nil {
		return err
	}
	if err := validateMigrationMode(cfg.MigrationMode); err != nil {
		return err
	}
	if err := validatePublicationMode(cfg.PublicationMode); err != nil {
		return err
	}
	if err := validateShadowReconciliation(cfg.ShadowReconciliation); err != nil {
		return err
	}
	return validateReviewExecution(cfg.ReviewExecution, cfg.ShadowReconciliation)
}

func validateDatabasePath(path string) error {
	if strings.TrimSpace(path) == "" || filepath.Clean(path) == "." {
		return errors.New("database path must identify a file")
	}
	return nil
}

func validateListenAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("listen address must include host and numeric port: %w", err)
	}
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("listen address host must be a loopback address")
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("listen address port must be a number from 1 to 65535")
	}
	return nil
}

func validateMigrationMode(mode MigrationMode) error {
	switch mode {
	case MigrationCheck, MigrationApply:
		return nil
	default:
		return fmt.Errorf("migration mode must be %q or %q", MigrationCheck, MigrationApply)
	}
}

func validatePublicationMode(mode PublicationMode) error {
	switch mode {
	case PublicationDisabled, PublicationSimulated:
		return nil
	default:
		return fmt.Errorf("publication mode must be %q or %q in this release", PublicationDisabled, PublicationSimulated)
	}
}

func validateShadowReconciliation(cfg ShadowReconciliationConfig) error {
	if cfg.Interval <= 0 {
		return errors.New("shadow reconciliation interval must be positive")
	}
	if !cfg.Enabled {
		return nil
	}
	if strings.TrimSpace(cfg.ConnectionID) == "" {
		return errors.New("shadow reconciliation connection ID is required when enabled")
	}
	if strings.TrimSpace(cfg.APIBaseURL) == "" {
		return errors.New("shadow reconciliation GitHub API URL is required when enabled")
	}
	if strings.TrimSpace(cfg.TokenEnvironment) == "" {
		return errors.New("shadow reconciliation token environment is required when enabled")
	}
	return nil
}

func parseEngineArgv(value string) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(value))
	var argv []string
	if err := decoder.Decode(&argv); err != nil || argv == nil {
		return nil, errors.New("must be a JSON argv array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("must contain one JSON argv array")
	} else if !errors.Is(err, io.EOF) {
		return nil, errors.New("must contain one JSON argv array")
	}
	for _, argument := range argv {
		if strings.ContainsRune(argument, 0) {
			return nil, errors.New("argv cannot contain NUL")
		}
	}
	return append([]string(nil), argv...), nil
}

func validateReviewExecution(review ReviewExecutionConfig, shadow ShadowReconciliationConfig) error {
	if !review.Enabled {
		return nil
	}
	if !shadow.Enabled {
		return errors.New("review execution requires enabled shadow reconciliation")
	}
	if len(review.EngineArgv) == 0 || strings.TrimSpace(review.EngineArgv[0]) == "" {
		return errors.New("review execution engine argv is required when enabled")
	}
	for _, argument := range review.EngineArgv {
		if strings.ContainsRune(argument, 0) {
			return errors.New("review execution engine argv cannot contain NUL")
		}
	}
	return nil
}
