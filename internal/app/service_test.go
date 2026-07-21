package app

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/hydrateworker"
	"github.com/sephriot/code-reviewer/internal/application/reconcile"
	"github.com/sephriot/code-reviewer/internal/application/reconcileworker"
	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"

	_ "modernc.org/sqlite"
)

type waitingRunner struct{ started chan<- struct{} }

type reconciliationSchedulerFunc func(context.Context, reconcile.Config) (sqlite.EnsureJobResult, error)

func (f reconciliationSchedulerFunc) Schedule(ctx context.Context, cfg reconcile.Config) (sqlite.EnsureJobResult, error) {
	return f(ctx, cfg)
}

type hydrationSchedulerFunc func(context.Context, string) ([]sqlite.EnsureJobResult, error)

func (f hydrationSchedulerFunc) Schedule(ctx context.Context, connectionID string) ([]sqlite.EnsureJobResult, error) {
	return f(ctx, connectionID)
}

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

func TestEnvironmentHydrationReaderFactoryUsesConfiguredReadOnlyConnection(t *testing.T) {
	t.Parallel()
	config := reconcile.Config{
		ConnectionID:      "connection-1",
		APIBaseURL:        "http://127.0.0.1:9999",
		CredentialRefKind: "environment",
		CredentialLocator: "TEST_GITHUB_TOKEN",
	}
	factory := environmentHydrationReaderFactory(func(name string) (string, bool) {
		if name != config.CredentialLocator {
			t.Fatalf("lookup name = %q", name)
		}
		return "not-persisted-token", true
	}, config)
	reader, err := factory(context.Background(), config.ConnectionID)
	if err != nil {
		t.Fatal(err)
	}
	if reader == nil {
		t.Fatal("reader = nil")
	}
	_, err = factory(context.Background(), "other-connection")
	if err == nil || !worker.IsPermanent(err) || !strings.Contains(err.Error(), "connection") {
		t.Fatalf("wrong connection error = %v", err)
	}
}

func TestScheduleShadowSchedulesReconciliationBeforeHydration(t *testing.T) {
	t.Parallel()
	calls := make([]string, 0, 2)
	config := reconcile.Config{ConnectionID: "connection-1"}
	reconciliation := reconciliationSchedulerFunc(func(_ context.Context, got reconcile.Config) (sqlite.EnsureJobResult, error) {
		if got != config {
			t.Fatalf("reconciliation config = %#v, want %#v", got, config)
		}
		calls = append(calls, "reconcile")
		return sqlite.EnsureJobResult{}, nil
	})
	hydration := hydrationSchedulerFunc(func(_ context.Context, connectionID string) ([]sqlite.EnsureJobResult, error) {
		if connectionID != config.ConnectionID {
			t.Fatalf("hydration connection = %q, want %q", connectionID, config.ConnectionID)
		}
		calls = append(calls, "hydrate")
		return nil, nil
	})
	if err := scheduleShadow(context.Background(), reconciliation, hydration, config); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(calls, ","), "reconcile,hydrate"; got != want {
		t.Fatalf("schedule order = %q, want %q", got, want)
	}
}

func TestNewRegistersBothShadowWorkerHandlers(t *testing.T) {
	cfg := config.Default()
	cfg.DatabasePath = filepath.Join(t.TempDir(), "control-plane.db")
	cfg.MigrationMode = config.MigrationApply
	service, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()

	runner, ok := service.jobRunner.(*worker.Runner)
	if !ok {
		t.Fatalf("jobRunner = %T, want *worker.Runner", service.jobRunner)
	}
	router, ok := runner.Handler.(*worker.Router)
	if !ok {
		t.Fatalf("worker handler = %T, want *worker.Router", runner.Handler)
	}
	for _, job := range []sqlite.Job{
		{Kind: reconcileworker.ReconcileJobKind, Payload: []byte(`{}`)},
		{Kind: hydrateworker.HydrateJobKind, Payload: []byte(`{}`)},
	} {
		err := router.Handle(context.Background(), job)
		if err == nil || !worker.IsPermanent(err) || strings.Contains(err.Error(), "unknown job kind") {
			t.Fatalf("router.Handle(%q) error = %v", job.Kind, err)
		}
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
