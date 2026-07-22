package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const maxGitHubWebhookPayloadBytes = 1 << 20

var (
	// ErrGitHubWebhookDeliveryConflict means a delivery ID was replayed with
	// different immutable metadata. Processing must fail closed.
	ErrGitHubWebhookDeliveryConflict = errors.New("github webhook delivery metadata conflict")
	githubWebhookDeliveryIDPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// RecordGitHubWebhookDeliveryInput contains only signed, bounded webhook
// metadata. It intentionally has no raw payload field.
type RecordGitHubWebhookDeliveryInput struct {
	DeliveryID         string
	EventType          string
	Action             string
	RepositoryGitHubID int64
	PullRequestNumber  int
	PayloadSHA256      string
	PayloadBytes       int
	ReceivedAt         time.Time
}

// GitHubWebhookDelivery is one retained verified webhook delivery.
type GitHubWebhookDelivery struct {
	DeliveryID         string
	EventType          string
	Action             string
	RepositoryGitHubID int64
	PullRequestNumber  int
	PayloadSHA256      string
	PayloadBytes       int
	ReceivedAt         time.Time
}

// RecordGitHubWebhookDeliveryResult reports whether metadata was newly
// retained. Duplicate identical deliveries are successful no-ops.
type RecordGitHubWebhookDeliveryResult struct {
	Delivery GitHubWebhookDelivery
	Created  bool
}

// RecordGitHubWebhookDelivery retains verified ingress metadata for durable
// idempotency. It does not create jobs, events, outbox rows, or provider work.
func (s *Store) RecordGitHubWebhookDelivery(ctx context.Context, input RecordGitHubWebhookDeliveryInput) (RecordGitHubWebhookDeliveryResult, error) {
	normalized, err := normalizeGitHubWebhookDeliveryInput(input)
	if err != nil {
		return RecordGitHubWebhookDeliveryResult{}, err
	}
	var result RecordGitHubWebhookDeliveryResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		inserted, err := conn.ExecContext(ctx, `
INSERT INTO github_webhook_deliveries(
    delivery_id, event_type, action, repository_github_id, pull_request_number,
    payload_sha256, payload_bytes, received_at_us
) VALUES (?, ?, ?, ?, NULLIF(?, 0), ?, ?, ?)
ON CONFLICT(delivery_id) DO NOTHING`,
			normalized.DeliveryID, normalized.EventType, normalized.Action, normalized.RepositoryGitHubID,
			normalized.PullRequestNumber, normalized.PayloadSHA256, normalized.PayloadBytes, normalized.ReceivedAt.UnixMicro())
		if err != nil {
			return fmt.Errorf("insert github webhook delivery: %w", err)
		}
		count, err := inserted.RowsAffected()
		if err != nil {
			return fmt.Errorf("read github webhook delivery insertion: %w", err)
		}
		stored, err := loadGitHubWebhookDelivery(ctx, conn, normalized.DeliveryID)
		if err != nil {
			return err
		}
		if !sameGitHubWebhookDeliveryFacts(stored, normalized) {
			return ErrGitHubWebhookDeliveryConflict
		}
		result = RecordGitHubWebhookDeliveryResult{Delivery: stored, Created: count == 1}
		return nil
	})
	if err != nil {
		return RecordGitHubWebhookDeliveryResult{}, fmt.Errorf("record github webhook delivery: %w", err)
	}
	return result, nil
}

func normalizeGitHubWebhookDeliveryInput(input RecordGitHubWebhookDeliveryInput) (RecordGitHubWebhookDeliveryInput, error) {
	input.DeliveryID = strings.TrimSpace(input.DeliveryID)
	input.EventType = strings.TrimSpace(input.EventType)
	input.Action = strings.TrimSpace(input.Action)
	input.PayloadSHA256 = strings.TrimSpace(input.PayloadSHA256)
	if !githubWebhookDeliveryIDPattern.MatchString(input.DeliveryID) || !validGitHubWebhookEventType(input.EventType) ||
		!validGitHubWebhookAction(input.Action) || input.RepositoryGitHubID <= 0 || input.PayloadBytes < 1 || input.PayloadBytes > maxGitHubWebhookPayloadBytes ||
		!validSHA256(input.PayloadSHA256) {
		return RecordGitHubWebhookDeliveryInput{}, errors.New("github webhook delivery input is invalid")
	}
	if input.EventType == "ping" {
		if input.PullRequestNumber != 0 {
			return RecordGitHubWebhookDeliveryInput{}, errors.New("github webhook ping cannot identify a pull request")
		}
	} else if input.PullRequestNumber <= 0 {
		return RecordGitHubWebhookDeliveryInput{}, errors.New("github webhook pull request number is invalid")
	}
	input.ReceivedAt = input.ReceivedAt.UTC()
	if input.ReceivedAt.IsZero() {
		input.ReceivedAt = time.Now().UTC()
	}
	if input.ReceivedAt.UnixMicro() < 0 {
		return RecordGitHubWebhookDeliveryInput{}, errors.New("github webhook received time is invalid")
	}
	return input, nil
}

func loadGitHubWebhookDelivery(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, deliveryID string) (GitHubWebhookDelivery, error) {
	var stored GitHubWebhookDelivery
	var pullRequestNumber sql.NullInt64
	var receivedAtUS int64
	err := queryer.QueryRowContext(ctx, `
SELECT delivery_id, event_type, action, repository_github_id, pull_request_number,
       payload_sha256, payload_bytes, received_at_us
FROM github_webhook_deliveries WHERE delivery_id = ?`, deliveryID).Scan(
		&stored.DeliveryID, &stored.EventType, &stored.Action, &stored.RepositoryGitHubID, &pullRequestNumber,
		&stored.PayloadSHA256, &stored.PayloadBytes, &receivedAtUS)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubWebhookDelivery{}, errors.New("github webhook delivery was not retained")
	}
	if err != nil {
		return GitHubWebhookDelivery{}, fmt.Errorf("load github webhook delivery: %w", err)
	}
	if pullRequestNumber.Valid {
		if pullRequestNumber.Int64 <= 0 || pullRequestNumber.Int64 > int64(^uint(0)>>1) {
			return GitHubWebhookDelivery{}, errors.New("stored github webhook pull request number is invalid")
		}
		stored.PullRequestNumber = int(pullRequestNumber.Int64)
	}
	if receivedAtUS < 0 {
		return GitHubWebhookDelivery{}, errors.New("stored github webhook time is invalid")
	}
	stored.ReceivedAt = time.UnixMicro(receivedAtUS).UTC()
	return stored, nil
}

func sameGitHubWebhookDeliveryFacts(stored GitHubWebhookDelivery, input RecordGitHubWebhookDeliveryInput) bool {
	return stored.DeliveryID == input.DeliveryID && stored.EventType == input.EventType && stored.Action == input.Action &&
		stored.RepositoryGitHubID == input.RepositoryGitHubID && stored.PullRequestNumber == input.PullRequestNumber &&
		stored.PayloadSHA256 == input.PayloadSHA256 && stored.PayloadBytes == input.PayloadBytes
}

func validGitHubWebhookEventType(eventType string) bool {
	switch eventType {
	case "ping", "pull_request", "pull_request_review":
		return true
	default:
		return false
	}
}

func validGitHubWebhookAction(action string) bool {
	if len(action) < 1 || len(action) > 64 {
		return false
	}
	for _, value := range []byte(action) {
		if value < '!' || value > '~' {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}
