package sqlite

import (
	"context"
	"testing"
	"time"
)

func TestCreateCutoverCheckpointRequiresDisabledPublication(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir()+"/control.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.ApplyMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	checkpoint, err := store.CreateCutoverCheckpoint(ctx, "before-cutover", now)
	if err != nil || checkpoint.DomainEventSequence != 0 || checkpoint.PublicationMode != PublicationModeDisabled {
		t.Fatalf("checkpoint=%+v err=%v", checkpoint, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE system_state SET value = 'simulated' WHERE key = 'publication_mode'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateCutoverCheckpoint(ctx, "not-safe", now); err == nil {
		t.Fatal("checkpoint accepted non-disabled publication")
	}
}
