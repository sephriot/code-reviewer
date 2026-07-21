// Package app wires the reviewd foundation service.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/sephriot/code-reviewer/internal/api"
	"github.com/sephriot/code-reviewer/internal/config"
	storagesqlite "github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

// Service owns the control-plane database and HTTP server.
type Service struct {
	store  *storagesqlite.Store
	server *http.Server
}

// New prepares a service and enforces migration policy before it can listen.
func New(ctx context.Context, cfg config.Config) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate configuration: %w", err)
	}
	legacy, err := storagesqlite.IsLegacyDatabase(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, fmt.Errorf("inspect database identity: %w", err)
	}
	if legacy {
		return nil, errors.New("refusing to use a legacy database as the v2 control-plane database")
	}
	store, err := storagesqlite.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*Service, error) {
		_ = store.Close()
		return nil, err
	}

	if cfg.MigrationMode == config.MigrationApply {
		if _, err := store.ApplyMigrations(ctx); err != nil {
			return closeOnError(fmt.Errorf("apply migrations: %w", err))
		}
	}
	status, err := store.SchemaStatus(ctx)
	if err != nil {
		return closeOnError(fmt.Errorf("read schema status: %w", err))
	}
	if status.Pending != 0 || status.Current != status.Latest {
		return closeOnError(fmt.Errorf(
			"database schema is not current: current=%d latest=%d pending=%d",
			status.Current,
			status.Latest,
			status.Pending,
		))
	}

	health := api.NewHealthHandler(api.Readiness{
		Ping: store.Ping,
		SchemaStatus: func(ctx context.Context) (api.SchemaStatus, error) {
			status, err := store.SchemaStatus(ctx)
			return api.SchemaStatus{
				Current: status.Current,
				Latest:  status.Latest,
				Pending: status.Pending,
			}, err
		},
	})
	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           health,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	return &Service{store: store, server: server}, nil
}

// Run listens until the context is canceled or the server fails.
func (s *Service) Run(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.server.Addr, err)
	}
	logger.Info("reviewd listening", "address", listener.Addr().String(), "publication_mode", "disabled")

	serveError := make(chan error, 1)
	go func() {
		serveError <- s.server.Serve(listener)
	}()

	select {
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve control API: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down control API: %w", err)
		}
		if err := <-serveError; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve control API during shutdown: %w", err)
		}
		return nil
	}
}

// Close releases service resources.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	return s.store.Close()
}
