package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEnsureJobReturnsMatchingActiveJob(t *testing.T) {
	ctx := context.Background()
	store := openMigratedJobStore(t, ctx)
	input := JobInput{
		Kind:         "hydrate_canonical_revision",
		ResourceType: "pull_request",
		ResourceID:   "pr_123",
		DedupeKey:    "hydrate:pr_123:head",
		Payload:      []byte(`{"head_sha":"abcdef"}`),
		AvailableAt:  time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC),
		MaxAttempts:  2,
	}

	created, err := store.EnsureJob(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || !created.Created {
		t.Fatalf("created result = %+v", created)
	}

	input.AvailableAt = input.AvailableAt.Add(time.Hour)
	input.Priority = 9
	existing, err := store.EnsureJob(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if existing.ID != created.ID || existing.Created {
		t.Fatalf("existing result = %+v, want id %q and Created=false", existing, created.ID)
	}
	assertTableCount(t, ctx, store.db, "jobs", 1)
}

func TestEnsureJobRejectsConflictingActiveFacts(t *testing.T) {
	ctx := context.Background()
	store := openMigratedJobStore(t, ctx)
	input := JobInput{
		Kind:         "hydrate_canonical_revision",
		ResourceType: "pull_request",
		ResourceID:   "pr_123",
		DedupeKey:    "hydrate:pr_123:head",
		Payload:      []byte(`{"head_sha":"abcdef"}`),
	}
	if _, err := store.EnsureJob(ctx, input); err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*JobInput){
		"resource type": func(candidate *JobInput) { candidate.ResourceType = "repository" },
		"resource id":   func(candidate *JobInput) { candidate.ResourceID = "pr_456" },
		"payload bytes": func(candidate *JobInput) { candidate.Payload = []byte(`{ "head_sha":"abcdef"}`) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := input
			mutate(&candidate)
			if _, err := store.EnsureJob(ctx, candidate); !errors.Is(err, ErrJobConflict) {
				t.Fatalf("EnsureJob conflict error = %v, want ErrJobConflict", err)
			}
		})
	}
	assertTableCount(t, ctx, store.db, "jobs", 1)
}

func TestEnsureJobCreatesAgainAfterJobIsTerminal(t *testing.T) {
	ctx := context.Background()
	store := openMigratedJobStore(t, ctx)
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	input := JobInput{Kind: "hydrate_canonical_revision", DedupeKey: "hydrate:pr_123:head", Payload: []byte(`{}`), AvailableAt: now}
	first, err := store.EnsureJob(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.ClaimJob(ctx, "worker", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteJob(ctx, job.ID, "worker", job.LeaseGeneration, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	second, err := store.EnsureJob(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Created || second.ID == first.ID {
		t.Fatalf("second result = %+v, want new job after terminal job %q", second, first.ID)
	}
	assertTableCount(t, ctx, store.db, "jobs", 2)
}

func TestEnsureJobDeduplicatesEveryActiveState(t *testing.T) {
	for _, state := range []string{"queued", "running", "retry_wait"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			store := openMigratedJobStore(t, ctx)
			input := JobInput{Kind: "hydrate_canonical_revision", DedupeKey: "hydrate:pr_123:head", Payload: []byte(`{}`)}
			first, err := store.EnsureJob(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(ctx, "UPDATE jobs SET state = ? WHERE id = ?", state, first.ID); err != nil {
				t.Fatal(err)
			}
			second, err := store.EnsureJob(ctx, input)
			if err != nil {
				t.Fatal(err)
			}
			if second.ID != first.ID || second.Created {
				t.Fatalf("result = %+v, want existing active job %q", second, first.ID)
			}
		})
	}
}

func TestEnsureJobIsConcurrentIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openMigratedJobStore(t, ctx)
	input := JobInput{Kind: "hydrate_canonical_revision", DedupeKey: "hydrate:pr_123:head", Payload: []byte(`{}`)}

	start := make(chan struct{})
	results := make(chan EnsureJobResult, 4)
	errors := make(chan error, 4)
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			result, err := store.EnsureJob(ctx, input)
			if err != nil {
				errors <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Fatalf("EnsureJob: %v", err)
	}

	var jobID string
	created := 0
	for result := range results {
		if jobID == "" {
			jobID = result.ID
		}
		if result.ID != jobID {
			t.Fatalf("job IDs differ: got %q, want %q", result.ID, jobID)
		}
		if result.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created count = %d, want 1", created)
	}
	assertTableCount(t, ctx, store.db, "jobs", 1)
}

func TestEnsureJobRequiresDedupeKey(t *testing.T) {
	ctx := context.Background()
	store := openMigratedJobStore(t, ctx)
	if _, err := store.EnsureJob(ctx, JobInput{Kind: "hydrate_canonical_revision"}); err == nil {
		t.Fatal("EnsureJob accepted an empty dedupe key")
	}
}

func openMigratedJobStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "control-plane.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ApplyMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}
