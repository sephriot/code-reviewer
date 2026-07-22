package config

import (
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	t.Parallel()

	got := Default()
	if got.DatabasePath != "data/control-plane.db" {
		t.Errorf("DatabasePath = %q, want data/control-plane.db", got.DatabasePath)
	}
	if got.ListenAddress != "127.0.0.1:8080" {
		t.Errorf("ListenAddress = %q, want 127.0.0.1:8080", got.ListenAddress)
	}
	if got.MigrationMode != MigrationCheck {
		t.Errorf("MigrationMode = %q, want %q", got.MigrationMode, MigrationCheck)
	}
	if got.PublicationMode != PublicationDisabled {
		t.Errorf("PublicationMode = %q, want %q", got.PublicationMode, PublicationDisabled)
	}
	if got.ShadowReconciliation.Enabled {
		t.Error("ShadowReconciliation.Enabled = true, want false")
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Default().Validate() error = %v", err)
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		EnvDatabasePath:                   "/var/lib/reviewd/control-plane.db",
		EnvListenAddress:                  "[::1]:9090",
		EnvMigrationMode:                  "apply",
		EnvPublicationMode:                "simulated",
		EnvShadowReconcileEnabled:         "true",
		EnvGitHubConnectionID:             "github:local",
		EnvGitHubAPIBaseURL:               "https://api.github.com",
		EnvGitHubTokenEnvironment:         "TEST_GITHUB_TOKEN",
		EnvShadowReconcileInterval:        "2m",
		EnvReviewExecutionEnabled:         "true",
		EnvReviewEngineArgv:               `["/usr/local/bin/review-engine","--json"]`,
		EnvGitHubWebhookEnabled:           "true",
		EnvGitHubWebhookSecretEnvironment: "TEST_GITHUB_WEBHOOK_SECRET",
	}
	got, err := Load(func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.DatabasePath != values[EnvDatabasePath] {
		t.Errorf("DatabasePath = %q", got.DatabasePath)
	}
	if got.ListenAddress != values[EnvListenAddress] {
		t.Errorf("ListenAddress = %q", got.ListenAddress)
	}
	if got.MigrationMode != MigrationApply {
		t.Errorf("MigrationMode = %q, want %q", got.MigrationMode, MigrationApply)
	}
	if got.PublicationMode != PublicationSimulated {
		t.Errorf("PublicationMode = %q, want %q", got.PublicationMode, PublicationSimulated)
	}
	if !got.ShadowReconciliation.Enabled || got.ShadowReconciliation.ConnectionID != "github:local" ||
		got.ShadowReconciliation.TokenEnvironment != "TEST_GITHUB_TOKEN" || got.ShadowReconciliation.Interval.String() != "2m0s" {
		t.Errorf("ShadowReconciliation = %#v", got.ShadowReconciliation)
	}
	if !got.ReviewExecution.Enabled || len(got.ReviewExecution.EngineArgv) != 2 ||
		got.ReviewExecution.EngineArgv[0] != "/usr/local/bin/review-engine" {
		t.Errorf("ReviewExecution = %#v", got.ReviewExecution)
	}
	if !got.GitHubWebhook.Enabled || got.GitHubWebhook.SecretEnvironment != "TEST_GITHUB_WEBHOOK_SECRET" {
		t.Errorf("GitHubWebhook = %#v", got.GitHubWebhook)
	}
}

func TestLoadRejectsMalformedEngineArgv(t *testing.T) {
	t.Parallel()

	_, err := Load(func(key string) (string, bool) {
		if key == EnvReviewEngineArgv {
			return `{"not":"argv"}`, true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), EnvReviewEngineArgv) {
		t.Fatalf("Load() error = %v, want error naming %s", err, EnvReviewEngineArgv)
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	_, err := Load(func(key string) (string, bool) {
		if key == EnvListenAddress {
			return "0.0.0.0:8080", true
		}
		return "", false
	})
	if err == nil || !strings.Contains(err.Error(), EnvListenAddress) {
		t.Fatalf("Load() error = %v, want error naming %s", err, EnvListenAddress)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*Config)
		wantError string
	}{
		{
			name: "enabled webhook missing signing-secret reference",
			mutate: func(cfg *Config) {
				cfg.GitHubWebhook.Enabled = true
			},
			wantError: "webhook secret environment",
		},
		{
			name: "enabled webhook has invalid signing-secret reference",
			mutate: func(cfg *Config) {
				cfg.GitHubWebhook = GitHubWebhookConfig{Enabled: true, SecretEnvironment: "secret value"}
			},
			wantError: "webhook secret environment",
		},
		{
			name: "empty database path",
			mutate: func(cfg *Config) {
				cfg.DatabasePath = ""
			},
			wantError: "database path",
		},
		{
			name: "database path is directory shorthand",
			mutate: func(cfg *Config) {
				cfg.DatabasePath = "."
			},
			wantError: "database path",
		},
		{
			name: "missing listener port",
			mutate: func(cfg *Config) {
				cfg.ListenAddress = "127.0.0.1"
			},
			wantError: "listen address",
		},
		{
			name: "wildcard listener",
			mutate: func(cfg *Config) {
				cfg.ListenAddress = "0.0.0.0:8080"
			},
			wantError: "loopback",
		},
		{
			name: "non-loopback listener",
			mutate: func(cfg *Config) {
				cfg.ListenAddress = "192.0.2.1:8080"
			},
			wantError: "loopback",
		},
		{
			name: "hostname listener",
			mutate: func(cfg *Config) {
				cfg.ListenAddress = "example.test:8080"
			},
			wantError: "loopback",
		},
		{
			name: "non-numeric port",
			mutate: func(cfg *Config) {
				cfg.ListenAddress = "127.0.0.1:http"
			},
			wantError: "port",
		},
		{
			name: "zero port",
			mutate: func(cfg *Config) {
				cfg.ListenAddress = "127.0.0.1:0"
			},
			wantError: "port",
		},
		{
			name: "unsupported migration mode",
			mutate: func(cfg *Config) {
				cfg.MigrationMode = "automatic"
			},
			wantError: "migration mode",
		},
		{
			name: "unknown publication mode",
			mutate: func(cfg *Config) {
				cfg.PublicationMode = "unknown"
			},
			wantError: "publication mode",
		},
		{
			name: "enabled reconciliation needs connection configuration",
			mutate: func(cfg *Config) {
				cfg.ShadowReconciliation.Enabled = true
			},
			wantError: "shadow reconciliation connection ID",
		},
		{
			name: "reconciliation interval must be positive",
			mutate: func(cfg *Config) {
				cfg.ShadowReconciliation.Interval = 0
			},
			wantError: "shadow reconciliation interval",
		},
		{
			name: "review execution needs the configured reader connection",
			mutate: func(cfg *Config) {
				cfg.ReviewExecution = ReviewExecutionConfig{Enabled: true, EngineArgv: []string{"review-engine"}}
			},
			wantError: "review execution requires enabled shadow reconciliation",
		},
		{
			name: "review execution needs an engine argv",
			mutate: func(cfg *Config) {
				cfg.ShadowReconciliation.Enabled = true
				cfg.ShadowReconciliation.ConnectionID = "github:local"
				cfg.ShadowReconciliation.TokenEnvironment = "TEST_GITHUB_TOKEN"
				cfg.ReviewExecution.Enabled = true
			},
			wantError: "review execution engine argv",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			test.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Validate() error = %v, want error containing %q", err, test.wantError)
			}
		})
	}
}

func TestValidateAcceptsLoopbackListeners(t *testing.T) {
	t.Parallel()

	for _, address := range []string{"127.0.0.1:1", "127.255.255.254:65535", "localhost:8080", "[::1]:8080"} {
		address := address
		t.Run(address, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.ListenAddress = address
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}
