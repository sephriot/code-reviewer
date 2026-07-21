// Package config defines the bootstrap configuration for reviewd.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
)

// Config contains startup-only control-plane settings.
type Config struct {
	DatabasePath    string          `json:"database_path"`
	ListenAddress   string          `json:"listen_address"`
	MigrationMode   MigrationMode   `json:"migration_mode"`
	PublicationMode PublicationMode `json:"publication_mode"`
}

// Default returns the safe local bootstrap configuration.
func Default() Config {
	return Config{
		DatabasePath:    filepath.Join("data", "control-plane.db"),
		ListenAddress:   "127.0.0.1:8080",
		MigrationMode:   MigrationCheck,
		PublicationMode: PublicationDisabled,
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
	return validatePublicationMode(cfg.PublicationMode)
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
	if mode != PublicationDisabled {
		return fmt.Errorf("publication mode must be %q in this release", PublicationDisabled)
	}
	return nil
}
