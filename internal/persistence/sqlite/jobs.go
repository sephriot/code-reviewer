package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// EnqueueJob stores a new queued job.
func (s *Store) EnqueueJob(ctx context.Context, input JobInput) (string, error) {
	if input.Kind == "" {
		return "", errors.New("job kind is required")
	}
	if len(input.Payload) == 0 {
		input.Payload = []byte(`{}`)
	}
	if !json.Valid(input.Payload) {
		return "", errors.New("job payload must be valid JSON")
	}
	if input.AvailableAt.IsZero() {
		input.AvailableAt = time.Now().UTC()
	}
	if input.MaxAttempts == 0 {
		input.MaxAttempts = 3
	}
	if input.MaxAttempts < 1 {
		return "", errors.New("job max attempts must be positive")
	}

	id, err := newID("job")
	if err != nil {
		return "", err
	}
	now := time.Now().UTC().UnixMicro()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO jobs(
    id, kind, resource_type, resource_id, dedupe_key, payload_json, state,
    priority, available_at_us, max_attempts, created_at_us, updated_at_us
) VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, 'queued', ?, ?, ?, ?, ?)`,
		id,
		input.Kind,
		input.ResourceType,
		input.ResourceID,
		input.DedupeKey,
		input.Payload,
		input.Priority,
		input.AvailableAt.UTC().UnixMicro(),
		input.MaxAttempts,
		now,
		now,
	)
	if err != nil {
		return "", fmt.Errorf("enqueue job: %w", err)
	}
	return id, nil
}

// ClaimJob claims the next eligible job for an owner.
func (s *Store) ClaimJob(ctx context.Context, owner string, now time.Time, lease time.Duration) (Job, error) {
	if owner == "" {
		return Job{}, errors.New("lease owner is required")
	}
	if lease <= 0 {
		return Job{}, errors.New("lease duration must be positive")
	}
	now = now.UTC()
	leaseExpires := now.Add(lease)
	if _, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET state = 'failed',
    lease_owner = NULL,
    lease_expires_at_us = NULL,
    last_error_class = 'lease_expired',
    last_error_message = 'lease expired after final attempt',
    updated_at_us = ?
WHERE state = 'running'
  AND lease_expires_at_us <= ?
  AND attempt >= max_attempts`, now.UnixMicro(), now.UnixMicro()); err != nil {
		return Job{}, fmt.Errorf("expire exhausted jobs: %w", err)
	}
	row := s.db.QueryRowContext(ctx, `
UPDATE jobs
SET state = 'running',
    lease_owner = ?,
    lease_expires_at_us = ?,
    lease_generation = lease_generation + 1,
    attempt = attempt + 1,
    updated_at_us = ?
WHERE id = (
    SELECT id
    FROM jobs
    WHERE attempt < max_attempts
      AND (
          (state IN ('queued', 'retry_wait') AND available_at_us <= ?)
          OR (state = 'running' AND lease_expires_at_us <= ?)
      )
    ORDER BY priority DESC, available_at_us, id
    LIMIT 1
)
RETURNING id, kind, payload_json, state, attempt, max_attempts,
          lease_owner, lease_generation, lease_expires_at_us`,
		owner,
		leaseExpires.UnixMicro(),
		now.UnixMicro(),
		now.UnixMicro(),
		now.UnixMicro(),
	)

	var job Job
	var leaseExpiresMicros int64
	if err := row.Scan(
		&job.ID,
		&job.Kind,
		&job.Payload,
		&job.State,
		&job.Attempt,
		&job.MaxAttempts,
		&job.LeaseOwner,
		&job.LeaseGeneration,
		&leaseExpiresMicros,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrNoJob
		}
		return Job{}, fmt.Errorf("claim job: %w", err)
	}
	job.LeaseExpiresAt = time.UnixMicro(leaseExpiresMicros).UTC()
	return job, nil
}

// HeartbeatJob extends a live lease while enforcing its fencing token.
func (s *Store) HeartbeatJob(
	ctx context.Context,
	id, owner string,
	generation int64,
	now time.Time,
	lease time.Duration,
) error {
	if lease <= 0 {
		return errors.New("lease duration must be positive")
	}
	now = now.UTC()
	result, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET lease_expires_at_us = ?, updated_at_us = ?
WHERE id = ?
  AND state = 'running'
  AND lease_owner = ?
  AND lease_generation = ?
  AND lease_expires_at_us > ?`,
		now.Add(lease).UnixMicro(),
		now.UnixMicro(),
		id,
		owner,
		generation,
		now.UnixMicro(),
	)
	if err != nil {
		return fmt.Errorf("heartbeat job: %w", err)
	}
	return requireSingleLeaseRow(result)
}

// FailJob records a fenced failure and either schedules a retry or terminates the job.
func (s *Store) FailJob(
	ctx context.Context,
	id, owner string,
	generation int64,
	now, retryAt time.Time,
	retry bool,
	errorClass, errorMessage string,
) error {
	now = now.UTC()
	retryState := "failed"
	if retry {
		retryState = "retry_wait"
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET state = CASE WHEN ? = 'retry_wait' AND attempt < max_attempts THEN 'retry_wait' ELSE 'failed' END,
    available_at_us = CASE WHEN ? = 'retry_wait' AND attempt < max_attempts THEN ? ELSE available_at_us END,
    lease_owner = NULL,
    lease_expires_at_us = NULL,
    last_error_class = NULLIF(?, ''),
    last_error_message = NULLIF(?, ''),
    updated_at_us = ?
WHERE id = ?
  AND state = 'running'
  AND lease_owner = ?
	AND lease_generation = ?
	AND lease_expires_at_us > ?`,
		retryState,
		retryState,
		retryAt.UTC().UnixMicro(),
		errorClass,
		errorMessage,
		now.UnixMicro(),
		id,
		owner,
		generation,
		now.UnixMicro(),
	)
	if err != nil {
		return fmt.Errorf("fail job: %w", err)
	}
	return requireSingleLeaseRow(result)
}

// CompleteJob completes a job only while the caller owns its fencing token.
func (s *Store) CompleteJob(ctx context.Context, id, owner string, generation int64, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET state = 'succeeded',
    lease_owner = NULL,
    lease_expires_at_us = NULL,
    updated_at_us = ?
WHERE id = ?
  AND state = 'running'
  AND lease_owner = ?
	AND lease_generation = ?
	AND lease_expires_at_us > ?`,
		now.UTC().UnixMicro(),
		id,
		owner,
		generation,
		now.UTC().UnixMicro(),
	)
	if err != nil {
		return fmt.Errorf("complete job: %w", err)
	}
	return requireSingleLeaseRow(result)
}

func requireSingleLeaseRow(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read fenced update result: %w", err)
	}
	if rows != 1 {
		return ErrLeaseLost
	}
	return nil
}

func newID(prefix string) (string, error) {
	random := make([]byte, 10)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate ID entropy: %w", err)
	}
	return prefix + "_" + strconv.FormatInt(time.Now().UTC().UnixMilli(), 36) + hex.EncodeToString(random), nil
}
