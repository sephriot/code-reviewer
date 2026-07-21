package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sephriot/code-reviewer/internal/app"
	"github.com/sephriot/code-reviewer/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadEnv()
	if err != nil {
		return err
	}
	flag.StringVar(&cfg.DatabasePath, "database", cfg.DatabasePath, "control-plane SQLite database")
	flag.StringVar(&cfg.ListenAddress, "listen", cfg.ListenAddress, "loopback listen address")
	migrationMode := string(cfg.MigrationMode)
	flag.StringVar(&migrationMode, "migrations", migrationMode, "migration mode: check or apply")
	flag.BoolVar(&cfg.ShadowReconciliation.Enabled, "shadow-reconcile", cfg.ShadowReconciliation.Enabled, "enable GET-only GitHub reconciliation")
	flag.StringVar(&cfg.ShadowReconciliation.ConnectionID, "github-connection-id", cfg.ShadowReconciliation.ConnectionID, "local GitHub connection ID")
	flag.StringVar(&cfg.ShadowReconciliation.APIBaseURL, "github-api-url", cfg.ShadowReconciliation.APIBaseURL, "GitHub API base URL")
	flag.StringVar(&cfg.ShadowReconciliation.TokenEnvironment, "github-token-environment", cfg.ShadowReconciliation.TokenEnvironment, "environment variable containing the GitHub token")
	flag.DurationVar(&cfg.ShadowReconciliation.Interval, "shadow-reconcile-interval", cfg.ShadowReconciliation.Interval, "shadow reconciliation enqueue interval")
	flag.Parse()
	cfg.MigrationMode = config.MigrationMode(migrationMode)
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	service, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	return service.Run(ctx, slog.Default())
}
