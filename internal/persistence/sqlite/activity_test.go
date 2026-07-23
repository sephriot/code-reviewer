package sqlite

import (
	"context"
	"strings"
	"testing"
)

func TestListActivityIncludesBoundedJobFailureReason(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	message := strings.Repeat("x", maxActivityErrorMessageBytes+50)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO jobs(
 id, kind, payload_json, state, available_at_us, attempt, max_attempts,
 last_error_class, last_error_message, created_at_us, updated_at_us)
VALUES ('job-1', 'github.reconcile.v1', '{}', 'failed', 1, 3, 3, 'permanent', ?, 1, 2)`, message); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListActivity(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items=%+v", items)
	}
	item := items[0]
	if item.Kind != "job" || item.State != "failed" || item.ErrorClass != "permanent" || len(item.ErrorMessage) != maxActivityErrorMessageBytes {
		t.Fatalf("item=%+v", item)
	}
}
