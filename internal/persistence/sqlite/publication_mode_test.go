package sqlite

import (
	"context"
	"testing"
	"time"
)

func TestSetPublicationModeChangesAndAuditsOnce(t *testing.T) {
	ctx := context.Background()
	store := openMigratedJobStore(t, ctx)
	now := time.Unix(100, 0).UTC()
	changed, err := store.SetPublicationMode(ctx, PublicationModeEnabled, now)
	if err != nil || !changed {
		t.Fatalf("SetPublicationMode() changed=%v err=%v", changed, err)
	}
	summary, err := store.SettingsSummary(ctx)
	if err != nil || summary.PublicationMode != PublicationModeEnabled {
		t.Fatalf("settings=%+v err=%v", summary, err)
	}
	var previous, next, source string
	var changedAt int64
	if err := store.db.QueryRowContext(ctx, `SELECT previous_mode, new_mode, source, changed_at_us FROM publication_mode_changes`).Scan(&previous, &next, &source, &changedAt); err != nil {
		t.Fatal(err)
	}
	if previous != "disabled" || next != "enabled" || source != "runtime_config" || changedAt != now.UnixMicro() {
		t.Fatalf("audit=%q,%q,%q,%d", previous, next, source, changedAt)
	}
	changed, err = store.SetPublicationMode(ctx, PublicationModeEnabled, now.Add(time.Second))
	if err != nil || changed {
		t.Fatalf("repeat SetPublicationMode() changed=%v err=%v", changed, err)
	}
	assertTableCount(t, ctx, store.db, "publication_mode_changes", 1)
}
