package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ClaimOutboxDelivery claims the next eligible event delivery with a fenced
// lease. Expired exhausted deliveries become failed before selection.
func (s *Store) ClaimOutboxDelivery(ctx context.Context, owner string, now time.Time, lease time.Duration) (OutboxDelivery, error) {
	if strings.TrimSpace(owner) == "" || lease <= 0 {
		return OutboxDelivery{}, errors.New("outbox lease input is invalid")
	}
	now = now.UTC()
	if _, err := s.db.ExecContext(ctx, `
UPDATE outbox
SET state = 'failed', lease_owner = NULL, lease_expires_at_us = NULL,
    last_error = 'lease expired after final attempt', updated_at_us = ?
WHERE state = 'delivering' AND lease_expires_at_us <= ? AND attempt >= max_attempts`, now.UnixMicro(), now.UnixMicro()); err != nil {
		return OutboxDelivery{}, fmt.Errorf("expire exhausted outbox deliveries: %w", err)
	}
	row := s.db.QueryRowContext(ctx, `
UPDATE outbox
SET state = 'delivering', lease_owner = ?, lease_expires_at_us = ?,
    lease_generation = lease_generation + 1, attempt = attempt + 1, updated_at_us = ?
WHERE id = (
    SELECT id FROM outbox
    WHERE attempt < max_attempts AND (
        (state IN ('pending', 'retry_wait') AND available_at_us <= ?)
        OR (state = 'delivering' AND lease_expires_at_us <= ?)
    )
    ORDER BY available_at_us, id LIMIT 1
)
RETURNING id, event_id, topic, payload_json, state, attempt, max_attempts,
          lease_owner, lease_generation, lease_expires_at_us`,
		owner, now.Add(lease).UnixMicro(), now.UnixMicro(), now.UnixMicro(), now.UnixMicro())
	var delivery OutboxDelivery
	var leaseExpiresAt int64
	if err := row.Scan(&delivery.ID, &delivery.EventID, &delivery.Topic, &delivery.Payload, &delivery.State, &delivery.Attempt, &delivery.MaxAttempts, &delivery.LeaseOwner, &delivery.LeaseGeneration, &leaseExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return OutboxDelivery{}, ErrNoOutboxDelivery
		}
		return OutboxDelivery{}, fmt.Errorf("claim outbox delivery: %w", err)
	}
	delivery.LeaseExpiresAt = time.UnixMicro(leaseExpiresAt).UTC()
	return delivery, nil
}

// HeartbeatOutboxDelivery extends a live outbox lease only for its owner and generation.
func (s *Store) HeartbeatOutboxDelivery(ctx context.Context, id, owner string, generation int64, now time.Time, lease time.Duration) error {
	if lease <= 0 {
		return errors.New("outbox lease duration must be positive")
	}
	now = now.UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE outbox SET lease_expires_at_us = ?, updated_at_us = ?
WHERE id = ? AND state = 'delivering' AND lease_owner = ? AND lease_generation = ?
  AND lease_expires_at_us > ?`, now.Add(lease).UnixMicro(), now.UnixMicro(), id, owner, generation, now.UnixMicro())
	if err != nil {
		return fmt.Errorf("heartbeat outbox delivery: %w", err)
	}
	return requireSingleLeaseRow(result)
}

// CompleteOutboxDelivery records successful local handling under its lease.
func (s *Store) CompleteOutboxDelivery(ctx context.Context, id, owner string, generation int64, now time.Time) error {
	now = now.UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE outbox
SET state = 'delivered', lease_owner = NULL, lease_expires_at_us = NULL, last_error = NULL, updated_at_us = ?
WHERE id = ? AND state = 'delivering' AND lease_owner = ? AND lease_generation = ? AND lease_expires_at_us > ?`, now.UnixMicro(), id, owner, generation, now.UnixMicro())
	if err != nil {
		return fmt.Errorf("complete outbox delivery: %w", err)
	}
	return requireSingleLeaseRow(result)
}

// FailOutboxDelivery records a bounded failure and optionally schedules retry.
func (s *Store) FailOutboxDelivery(ctx context.Context, id, owner string, generation int64, now, retryAt time.Time, retry bool, message string) error {
	now = now.UTC()
	state := "failed"
	if retry {
		state = "retry_wait"
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE outbox
SET state = CASE WHEN ? = 'retry_wait' AND attempt < max_attempts THEN 'retry_wait' ELSE 'failed' END,
    available_at_us = CASE WHEN ? = 'retry_wait' AND attempt < max_attempts THEN ? ELSE available_at_us END,
    lease_owner = NULL, lease_expires_at_us = NULL, last_error = NULLIF(?, ''), updated_at_us = ?
WHERE id = ? AND state = 'delivering' AND lease_owner = ? AND lease_generation = ? AND lease_expires_at_us > ?`,
		state, state, retryAt.UTC().UnixMicro(), strings.TrimSpace(message), now.UnixMicro(), id, owner, generation, now.UnixMicro())
	if err != nil {
		return fmt.Errorf("fail outbox delivery: %w", err)
	}
	return requireSingleLeaseRow(result)
}
