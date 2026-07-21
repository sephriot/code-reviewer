package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrIdempotencyConflict means an event ID was reused with different content.
var ErrIdempotencyConflict = errors.New("idempotency conflict")

// DomainEventInput describes one immutable domain event.
type DomainEventInput struct {
	ID            string
	AggregateType string
	AggregateID   string
	EventType     string
	EventVersion  int
	Payload       []byte
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
}

// OutboxInput describes one delivery created from a domain event.
type OutboxInput struct {
	Topic       string
	Payload     []byte
	AvailableAt time.Time
	MaxAttempts int
}

// AppendedEvent identifies the event and linked outbox rows committed together.
type AppendedEvent struct {
	EventID   string
	Sequence  int64
	OutboxIDs []string
}

// AggregateMutation changes aggregate projections within a job-result transaction.
type AggregateMutation func(context.Context, *sql.Tx) error

// AppendEventWithOutbox atomically persists an immutable event and its deliveries.
func (s *Store) AppendEventWithOutbox(
	ctx context.Context,
	event DomainEventInput,
	outbox []OutboxInput,
) (AppendedEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AppendedEvent{}, fmt.Errorf("begin event transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := appendEventWithOutboxTx(ctx, tx, event, outbox)
	if err != nil {
		return AppendedEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return AppendedEvent{}, fmt.Errorf("commit event transaction: %w", err)
	}
	return result, nil
}

// AppendEventWithOutboxForJob persists an event only while the worker owns a live job lease.
func (s *Store) AppendEventWithOutboxForJob(
	ctx context.Context,
	jobID string,
	leaseOwner string,
	leaseGeneration int64,
	event DomainEventInput,
	outbox []OutboxInput,
) (AppendedEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AppendedEvent{}, fmt.Errorf("begin fenced event transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := verifyJobLease(ctx, tx, jobID, leaseOwner, leaseGeneration, time.Now().UTC()); err != nil {
		return AppendedEvent{}, err
	}

	result, err := appendEventWithOutboxTx(ctx, tx, event, outbox)
	if err != nil {
		return AppendedEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return AppendedEvent{}, fmt.Errorf("commit fenced event transaction: %w", err)
	}
	return result, nil
}

// CommitJobResult atomically mutates an aggregate, emits its event, and completes the owning job.
func (s *Store) CommitJobResult(
	ctx context.Context,
	jobID string,
	leaseOwner string,
	leaseGeneration int64,
	mutate AggregateMutation,
	event DomainEventInput,
	outbox []OutboxInput,
) (AppendedEvent, error) {
	if mutate == nil {
		return AppendedEvent{}, errors.New("aggregate mutation is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AppendedEvent{}, fmt.Errorf("begin job result transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if existing, found, err := existingJobResultTx(ctx, tx, jobID, event, outbox); err != nil {
		return AppendedEvent{}, err
	} else if found {
		return existing, nil
	}
	if err := verifyJobLease(ctx, tx, jobID, leaseOwner, leaseGeneration, time.Now().UTC()); err != nil {
		return AppendedEvent{}, err
	}
	if err := mutate(ctx, tx); err != nil {
		return AppendedEvent{}, fmt.Errorf("mutate aggregate: %w", err)
	}
	result, err := appendEventWithOutboxTx(ctx, tx, event, outbox)
	if err != nil {
		return AppendedEvent{}, err
	}
	if err := completeJobTx(ctx, tx, jobID, leaseOwner, leaseGeneration, event.ID, time.Now().UTC()); err != nil {
		return AppendedEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return AppendedEvent{}, fmt.Errorf("commit job result transaction: %w", err)
	}
	return result, nil
}

func appendEventWithOutboxTx(
	ctx context.Context,
	tx *sql.Tx,
	event DomainEventInput,
	outbox []OutboxInput,
) (AppendedEvent, error) {
	if err := validateDomainEvent(event); err != nil {
		return AppendedEvent{}, err
	}
	if len(outbox) == 0 {
		return AppendedEvent{}, errors.New("at least one outbox delivery is required")
	}
	for i, delivery := range outbox {
		if err := validateOutboxInput(delivery); err != nil {
			return AppendedEvent{}, fmt.Errorf("validate outbox delivery %d: %w", i, err)
		}
	}

	existing, found, err := existingEventTx(ctx, tx, event, outbox)
	if err != nil {
		return AppendedEvent{}, err
	}
	if found {
		return existing, nil
	}

	eventID := event.ID
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}

	inserted, err := tx.ExecContext(ctx, `
INSERT INTO domain_events(
    id, aggregate_type, aggregate_id, event_type, event_version, payload_json,
    correlation_id, causation_id, occurred_at_us
) VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)`,
		eventID,
		event.AggregateType,
		event.AggregateID,
		event.EventType,
		event.EventVersion,
		event.Payload,
		event.CorrelationID,
		event.CausationID,
		event.OccurredAt.UTC().UnixMicro(),
	)
	if err != nil {
		return AppendedEvent{}, fmt.Errorf("insert domain event: %w", err)
	}
	sequence, err := inserted.LastInsertId()
	if err != nil {
		return AppendedEvent{}, fmt.Errorf("read domain event sequence: %w", err)
	}

	result := AppendedEvent{
		EventID:   eventID,
		Sequence:  sequence,
		OutboxIDs: make([]string, 0, len(outbox)),
	}
	createdAt := time.Now().UTC()
	for i, delivery := range outbox {
		if delivery.AvailableAt.IsZero() {
			delivery.AvailableAt = createdAt
		}
		if delivery.MaxAttempts == 0 {
			delivery.MaxAttempts = 10
		}

		outboxID, err := newID("outbox")
		if err != nil {
			return AppendedEvent{}, fmt.Errorf("create outbox delivery %d ID: %w", i, err)
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO outbox(
    id, event_id, topic, payload_json, state, available_at_us, max_attempts,
    created_at_us, updated_at_us
) VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
			outboxID,
			eventID,
			delivery.Topic,
			delivery.Payload,
			delivery.AvailableAt.UTC().UnixMicro(),
			delivery.MaxAttempts,
			createdAt.UnixMicro(),
			createdAt.UnixMicro(),
		)
		if err != nil {
			return AppendedEvent{}, fmt.Errorf("insert outbox delivery %d: %w", i, err)
		}
		result.OutboxIDs = append(result.OutboxIDs, outboxID)
	}

	return result, nil
}

func verifyJobLease(
	ctx context.Context,
	tx *sql.Tx,
	jobID string,
	leaseOwner string,
	leaseGeneration int64,
	now time.Time,
) error {
	fence, err := tx.ExecContext(ctx, `
UPDATE jobs
SET lease_generation = lease_generation
WHERE id = ?
  AND state = 'running'
  AND lease_owner = ?
  AND lease_generation = ?
  AND lease_expires_at_us > ?`,
		jobID,
		leaseOwner,
		leaseGeneration,
		now.UTC().UnixMicro(),
	)
	if err != nil {
		return fmt.Errorf("verify job lease: %w", err)
	}
	matched, err := fence.RowsAffected()
	if err != nil {
		return fmt.Errorf("read job lease verification: %w", err)
	}
	if matched != 1 {
		return ErrLeaseLost
	}
	return nil
}

func completeJobTx(
	ctx context.Context,
	tx *sql.Tx,
	jobID string,
	leaseOwner string,
	leaseGeneration int64,
	eventID string,
	now time.Time,
) error {
	completed, err := tx.ExecContext(ctx, `
UPDATE jobs
SET state = 'succeeded',
    lease_owner = NULL,
    lease_expires_at_us = NULL,
    result_event_id = ?,
    updated_at_us = ?
WHERE id = ?
  AND state = 'running'
  AND lease_owner = ?
  AND lease_generation = ?
  AND lease_expires_at_us > ?`,
		eventID,
		now.UTC().UnixMicro(),
		jobID,
		leaseOwner,
		leaseGeneration,
		now.UTC().UnixMicro(),
	)
	if err != nil {
		return fmt.Errorf("complete job result: %w", err)
	}
	matched, err := completed.RowsAffected()
	if err != nil {
		return fmt.Errorf("read job result completion: %w", err)
	}
	if matched != 1 {
		return ErrLeaseLost
	}
	return nil
}

func existingJobResultTx(
	ctx context.Context,
	tx *sql.Tx,
	jobID string,
	event DomainEventInput,
	outbox []OutboxInput,
) (AppendedEvent, bool, error) {
	if event.ID == "" {
		return AppendedEvent{}, false, errors.New("event ID is required")
	}
	var state string
	var resultEventID sql.NullString
	err := tx.QueryRowContext(ctx, "SELECT state, result_event_id FROM jobs WHERE id = ?", jobID).Scan(&state, &resultEventID)
	if errors.Is(err, sql.ErrNoRows) {
		return AppendedEvent{}, false, ErrLeaseLost
	}
	if err != nil {
		return AppendedEvent{}, false, fmt.Errorf("read existing job result: %w", err)
	}
	if state != "succeeded" {
		var ownerJobID string
		err := tx.QueryRowContext(ctx, `
SELECT id
FROM jobs
WHERE result_event_id = ? AND id <> ?
LIMIT 1`, event.ID, jobID).Scan(&ownerJobID)
		if err == nil {
			return AppendedEvent{}, false, fmt.Errorf(
				"%w: event %q already belongs to job %q",
				ErrIdempotencyConflict,
				event.ID,
				ownerJobID,
			)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return AppendedEvent{}, false, fmt.Errorf("check event result ownership: %w", err)
		}
		return AppendedEvent{}, false, nil
	}
	if !resultEventID.Valid || resultEventID.String != event.ID {
		return AppendedEvent{}, false, fmt.Errorf("%w: job %q result event differs", ErrIdempotencyConflict, jobID)
	}
	existing, found, err := existingEventTx(ctx, tx, event, outbox)
	if err != nil {
		return AppendedEvent{}, false, err
	}
	if !found {
		return AppendedEvent{}, false, fmt.Errorf("job %q result event %q is missing", jobID, event.ID)
	}
	return existing, true, nil
}

func existingEventTx(
	ctx context.Context,
	tx *sql.Tx,
	event DomainEventInput,
	outbox []OutboxInput,
) (AppendedEvent, bool, error) {
	var stored struct {
		sequence      int64
		aggregateType string
		aggregateID   string
		eventType     string
		eventVersion  int
		payload       []byte
		correlationID sql.NullString
		causationID   sql.NullString
		occurredAtUS  int64
	}
	err := tx.QueryRowContext(ctx, `
SELECT sequence, aggregate_type, aggregate_id, event_type, event_version,
       payload_json, correlation_id, causation_id, occurred_at_us
FROM domain_events
WHERE id = ?`, event.ID).Scan(
		&stored.sequence,
		&stored.aggregateType,
		&stored.aggregateID,
		&stored.eventType,
		&stored.eventVersion,
		&stored.payload,
		&stored.correlationID,
		&stored.causationID,
		&stored.occurredAtUS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AppendedEvent{}, false, nil
	}
	if err != nil {
		return AppendedEvent{}, false, fmt.Errorf("read idempotent event: %w", err)
	}
	if stored.aggregateType != event.AggregateType ||
		stored.aggregateID != event.AggregateID ||
		stored.eventType != event.EventType ||
		stored.eventVersion != event.EventVersion ||
		!bytes.Equal(stored.payload, event.Payload) ||
		stored.correlationID.String != event.CorrelationID ||
		stored.causationID.String != event.CausationID ||
		(!event.OccurredAt.IsZero() && stored.occurredAtUS != event.OccurredAt.UTC().UnixMicro()) {
		return AppendedEvent{}, false, fmt.Errorf("%w: event %q content differs", ErrIdempotencyConflict, event.ID)
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id, topic, payload_json, available_at_us, max_attempts
FROM outbox
WHERE event_id = ?
ORDER BY rowid`, event.ID)
	if err != nil {
		return AppendedEvent{}, false, fmt.Errorf("read idempotent outbox: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0, len(outbox))
	index := 0
	for rows.Next() {
		var id, topic string
		var payload []byte
		var availableAtUS int64
		var maxAttempts int
		if err := rows.Scan(&id, &topic, &payload, &availableAtUS, &maxAttempts); err != nil {
			return AppendedEvent{}, false, fmt.Errorf("scan idempotent outbox: %w", err)
		}
		if index >= len(outbox) || !sameOutboxInput(outbox[index], topic, payload, availableAtUS, maxAttempts) {
			return AppendedEvent{}, false, fmt.Errorf("%w: event %q outbox differs", ErrIdempotencyConflict, event.ID)
		}
		ids = append(ids, id)
		index++
	}
	if err := rows.Err(); err != nil {
		return AppendedEvent{}, false, fmt.Errorf("iterate idempotent outbox: %w", err)
	}
	if index != len(outbox) {
		return AppendedEvent{}, false, fmt.Errorf("%w: event %q outbox count differs", ErrIdempotencyConflict, event.ID)
	}
	return AppendedEvent{EventID: event.ID, Sequence: stored.sequence, OutboxIDs: ids}, true, nil
}

func sameOutboxInput(input OutboxInput, topic string, payload []byte, availableAtUS int64, maxAttempts int) bool {
	wantMaxAttempts := input.MaxAttempts
	if wantMaxAttempts == 0 {
		wantMaxAttempts = 10
	}
	return topic == input.Topic &&
		bytes.Equal(payload, input.Payload) &&
		(input.AvailableAt.IsZero() || availableAtUS == input.AvailableAt.UTC().UnixMicro()) &&
		maxAttempts == wantMaxAttempts
}

func validateDomainEvent(event DomainEventInput) error {
	switch {
	case event.ID == "":
		return errors.New("event ID is required")
	case event.AggregateType == "":
		return errors.New("event aggregate type is required")
	case event.AggregateID == "":
		return errors.New("event aggregate ID is required")
	case event.EventType == "":
		return errors.New("event type is required")
	case event.EventVersion < 1:
		return errors.New("event version must be positive")
	case !json.Valid(event.Payload):
		return errors.New("event payload must be valid JSON")
	default:
		return nil
	}
}

func validateOutboxInput(delivery OutboxInput) error {
	switch {
	case delivery.Topic == "":
		return errors.New("outbox topic is required")
	case !json.Valid(delivery.Payload):
		return errors.New("outbox payload must be valid JSON")
	case delivery.MaxAttempts < 0:
		return errors.New("outbox max attempts must be positive")
	default:
		return nil
	}
}
