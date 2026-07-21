package app

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/reconcile"
	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/worker"

	_ "modernc.org/sqlite"
)

type waitingRunner struct{ started chan<- struct{} }

func (r waitingRunner) Run(ctx context.Context) error {
	r.started <- struct{}{}
	<-ctx.Done()
	return nil
}

func TestNewRequiresCurrentSchemaInCheckMode(t *testing.T) {
	cfg := config.Default()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "control-plane.db")
	cfg.MigrationMode = config.MigrationCheck

	service, err := New(context.Background(), cfg)
	if service != nil {
		_ = service.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "schema is not current") {
		t.Fatalf("New() error = %v", err)
	}
}

func TestNewRefusesLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"pr_reviews", "pending_approvals", "own_prs", "review_requests", "review_request_sync_state", "review_started_comments"} {
		if _, err := db.Exec("CREATE TABLE " + table + " (id INTEGER PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DatabasePath = path
	cfg.MigrationMode = config.MigrationApply
	service, err := New(context.Background(), cfg)
	if service != nil {
		_ = service.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "legacy database") {
		t.Fatalf("New() error = %v", err)
	}
}

func TestNewAppliesMigrationsOnlyWhenConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "control-plane.db")
	cfg.MigrationMode = config.MigrationApply

	service, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	cfg.MigrationMode = config.MigrationCheck
	service, err = New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEnvironmentReaderFactoryUsesOnlyNamedEnvironmentReference(t *testing.T) {
	t.Parallel()
	factory := environmentReaderFactory(func(name string) (string, bool) {
		if name != "TEST_GITHUB_TOKEN" {
			t.Fatalf("lookup name = %q", name)
		}
		return "not-persisted-token", true
	})
	reader, err := factory(context.Background(), reconcile.Config{
		APIBaseURL:        "http://127.0.0.1:9999",
		CredentialRefKind: "environment",
		CredentialLocator: "TEST_GITHUB_TOKEN",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reader == nil {
		t.Fatal("reader = nil")
	}
}

func TestEnvironmentReaderFactoryRejectsMissingTokenAsPermanent(t *testing.T) {
	t.Parallel()
	factory := environmentReaderFactory(func(string) (string, bool) { return "", false })
	_, err := factory(context.Background(), reconcile.Config{
		APIBaseURL:        "https://api.github.com",
		CredentialRefKind: "environment",
		CredentialLocator: "TEST_GITHUB_TOKEN",
	})
	if err == nil || !worker.IsPermanent(err) || strings.Contains(err.Error(), "not-persisted-token") {
		t.Fatalf("factory error = %v", err)
	}
}

func TestRunSchedulerStopsAfterCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := runScheduler(ctx, time.Hour, func(context.Context) error {
		calls++
		cancel()
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("runScheduler() error = %v, calls = %d", err, calls)
	}
}

func TestServiceRunCancelsWorkerAndScheduler(t *testing.T) {
	startedWorker := make(chan struct{}, 1)
	startedScheduler := make(chan struct{}, 1)
	service := &Service{
		server:           &http.Server{Addr: "127.0.0.1:0", Handler: http.NewServeMux()},
		jobRunner:        waitingRunner{started: startedWorker},
		scheduleInterval: time.Hour,
		schedule: func(context.Context) error {
			startedScheduler <- struct{}{}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx, nil) }()
	select {
	case <-startedWorker:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not start")
	}
	select {
	case <-startedScheduler:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not stop")
	}
}

func TestRunSchedulerRejectsInvalidInterval(t *testing.T) {
	t.Parallel()
	err := runScheduler(context.Background(), 0, func(context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "schedule interval") {
		t.Fatalf("runScheduler() error = %v", err)
	}
}
