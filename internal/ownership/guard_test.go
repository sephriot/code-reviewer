package ownership

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGuardExcludesSecondWriterAndAdvancesGeneration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	first, err := Acquire(ctx, dir, "reviewd-a", "cutover-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Valid(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(ctx, dir, "reviewd-b", "cutover-2", now); !errors.Is(err, ErrHeld) {
		t.Fatalf("second acquire error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(ctx, dir, "reviewd-b", "cutover-2", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.generation != 2 {
		t.Fatalf("generation = %d, want 2", second.generation)
	}
}

func TestGuardHeartbeatRequiresCurrentGeneration(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	guard, err := Acquire(ctx, t.TempDir(), "reviewd-a", "cutover-1", now)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if err := guard.Heartbeat(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.db.ExecContext(ctx, `UPDATE writer_ownership SET generation = generation + 1 WHERE singleton = 1`); err != nil {
		t.Fatal(err)
	}
	if err := guard.Heartbeat(ctx, now.Add(2*time.Minute)); err == nil {
		t.Fatal("stale owner heartbeat succeeded")
	}
}
