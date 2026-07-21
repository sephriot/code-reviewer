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
