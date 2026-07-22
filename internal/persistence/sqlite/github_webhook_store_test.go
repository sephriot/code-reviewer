package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGitHubWebhookDeliveryRecordsMetadataIdempotentlyWithoutRawPayload(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	input := RecordGitHubWebhookDeliveryInput{
		DeliveryID: "123e4567-e89b-12d3-a456-426614174000", EventType: "pull_request", Action: "opened",
		RepositoryGitHubID: 12345, PullRequestNumber: 42,
		PayloadSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		PayloadBytes:  88, ReceivedAt: time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC),
	}
	first, err := store.RecordGitHubWebhookDelivery(ctx, input)
	if err != nil || !first.Created || first.Delivery.DeliveryID != input.DeliveryID || first.Delivery.PayloadSHA256 != input.PayloadSHA256 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := store.RecordGitHubWebhookDelivery(ctx, input)
	if err != nil || second.Created || !second.Delivery.ReceivedAt.Equal(input.ReceivedAt) {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	changed := input
	changed.Action = "closed"
	if _, err := store.RecordGitHubWebhookDelivery(ctx, changed); !errors.Is(err, ErrGitHubWebhookDeliveryConflict) {
		t.Fatalf("changed metadata error=%v", err)
	}
	var rawPayloadColumns int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('github_webhook_deliveries') WHERE name LIKE '%payload_json%' OR name LIKE '%raw%'`).Scan(&rawPayloadColumns); err != nil || rawPayloadColumns != 0 {
		t.Fatalf("raw payload columns=%d err=%v", rawPayloadColumns, err)
	}
}
