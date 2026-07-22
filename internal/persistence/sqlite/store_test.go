package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	applied, err := store.ApplyMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 10 || applied[0] != 1 || applied[1] != 2 || applied[2] != 3 || applied[3] != 4 || applied[4] != 5 || applied[5] != 6 || applied[6] != 7 || applied[7] != 8 || applied[8] != 9 || applied[9] != 10 {
		t.Fatalf("first migration result = %v, want [1 2 3 4 5 6 7 8 9 10]", applied)
	}

	applied, err = store.ApplyMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second migration result = %v, want no changes", applied)
	}

	status, err := store.SchemaStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Current != 10 || status.Latest != 10 || status.Pending != 0 {
		t.Fatalf("schema status = %+v", status)
	}
}

func TestMigrationPreservesLegacyTables(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-copy.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.db.ExecContext(ctx, `
CREATE TABLE pr_reviews (id INTEGER PRIMARY KEY, review_comment TEXT);
INSERT INTO pr_reviews(id, review_comment) VALUES (7, 'preserve me')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var id int
	var comment string
	if err := store.db.QueryRowContext(ctx, "SELECT id, review_comment FROM pr_reviews").Scan(&id, &comment); err != nil {
		t.Fatal(err)
	}
	if id != 7 || comment != "preserve me" {
		t.Fatalf("legacy row changed: id=%d comment=%q", id, comment)
	}
}

func TestMigrationChecksumMismatchFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ApplyMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE schema_migrations SET checksum = 'changed' WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyMigrations(ctx); err == nil {
		t.Fatal("changed migration checksum was accepted")
	}
}

func TestUnknownMigrationVersionFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ApplyMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, checksum, applied_at_us) VALUES (99, 'future.sql', 'future', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SchemaStatus(ctx); err == nil {
		t.Fatal("unknown migration version was accepted")
	}
}

func TestImmediateTransactionRollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	wantErr := errors.New("stop")
	err = withImmediateConnection(ctx, store.db, func(conn *sql.Conn) error {
		if _, execErr := conn.ExecContext(ctx, "CREATE TABLE must_rollback(id INTEGER)"); execErr != nil {
			return execErr
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("transaction error = %v", err)
	}
	var exists int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE name='must_rollback'").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatal("failed migration transaction was not rolled back")
	}
}

func TestOpenSecuresDatabasePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-plane.db")
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if permissions := info.Mode().Perm(); permissions != 0o600 {
		t.Fatalf("database permissions = %o, want 600", permissions)
	}
}

func TestJobLeaseGenerationFencesStaleWorker(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ApplyMigrations(ctx); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	jobID, err := store.EnqueueJob(ctx, JobInput{
		Kind:        "test",
		DedupeKey:   "one",
		Payload:     []byte(`{"value":1}`),
		AvailableAt: now,
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.ClaimJob(ctx, "worker-a", now, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != jobID || first.LeaseGeneration != 1 {
		t.Fatalf("first claim = %+v", first)
	}

	second, err := store.ClaimJob(ctx, "worker-b", now.Add(2*time.Second), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != jobID || second.LeaseGeneration != 2 {
		t.Fatalf("second claim = %+v", second)
	}

	err = store.CompleteJob(ctx, jobID, "worker-a", first.LeaseGeneration, now.Add(2*time.Second))
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale completion error = %v, want ErrLeaseLost", err)
	}

	if err := store.CompleteJob(ctx, jobID, "worker-b", second.LeaseGeneration, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentClaimHasOneWinner(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ApplyMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	if _, err := store.EnqueueJob(ctx, JobInput{Kind: "test", AvailableAt: now}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, owner := range []string{"worker-a", "worker-b"} {
		group.Add(1)
		go func(owner string) {
			defer group.Done()
			<-start
			_, claimErr := store.ClaimJob(ctx, owner, now, time.Minute)
			results <- claimErr
		}(owner)
	}
	close(start)
	group.Wait()
	close(results)

	winners := 0
	noJob := 0
	for claimErr := range results {
		switch {
		case claimErr == nil:
			winners++
		case errors.Is(claimErr, ErrNoJob):
			noJob++
		default:
			t.Fatalf("unexpected claim error: %v", claimErr)
		}
	}
	if winners != 1 || noJob != 1 {
		t.Fatalf("claim results: winners=%d unavailable=%d", winners, noJob)
	}
}

func TestHeartbeatAndFailureAreFenced(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ApplyMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	if _, err := store.EnqueueJob(ctx, JobInput{Kind: "test", AvailableAt: now, MaxAttempts: 2}); err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimJob(ctx, "worker-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HeartbeatJob(ctx, job.ID, "worker-a", job.LeaseGeneration, now.Add(30*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.HeartbeatJob(ctx, job.ID, "worker-b", job.LeaseGeneration, now.Add(30*time.Second), time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong-owner heartbeat = %v", err)
	}
	if err := store.FailJob(ctx, job.ID, "worker-a", job.LeaseGeneration, now.Add(40*time.Second), now.Add(time.Hour), true, "transient", "retry"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, "worker-b", now.Add(30*time.Minute), time.Minute); !errors.Is(err, ErrNoJob) {
		t.Fatalf("early retry claim = %v", err)
	}
	second, err := store.ClaimJob(ctx, "worker-b", now.Add(2*time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.Attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", second.Attempt)
	}
	if err := store.FailJob(ctx, second.ID, "worker-b", second.LeaseGeneration, now.Add(2*time.Hour), now.Add(3*time.Hour), true, "transient", "again"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimJob(ctx, "worker-c", now.Add(4*time.Hour), time.Minute); !errors.Is(err, ErrNoJob) {
		t.Fatalf("exhausted job claim = %v, want ErrNoJob", err)
	}
}

func TestExpiredLeaseCannotCompleteOrFailBeforeReclaim(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ApplyMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	jobID, err := store.EnqueueJob(ctx, JobInput{Kind: "test", AvailableAt: now})
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimJob(ctx, "worker-a", now, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	afterExpiry := now.Add(2 * time.Second)
	if err := store.CompleteJob(ctx, jobID, "worker-a", job.LeaseGeneration, afterExpiry); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired completion = %v, want ErrLeaseLost", err)
	}
	if err := store.FailJob(ctx, jobID, "worker-a", job.LeaseGeneration, afterExpiry, afterExpiry.Add(time.Minute), true, "transient", "late"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired failure = %v, want ErrLeaseLost", err)
	}
}
