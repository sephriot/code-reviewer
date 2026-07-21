package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxReviewRunDiagnosticPayloadBytes = 256

var (
	// ErrReviewRunEventConflict means a terminal lifecycle outcome already
	// exists and does not exactly match the requested outcome.
	ErrReviewRunEventConflict = errors.New("review run terminal event conflicts with existing outcome")
	// ErrReviewRunSucceeded means assessment output has already made the run
	// terminally successful, so failure and cancellation cannot be appended.
	ErrReviewRunSucceeded = errors.New("review run already succeeded")
)

const (
	// ReviewRunEventFailedRetryable records a bounded failure that may be tried
	// again by a later run attempt.
	ReviewRunEventFailedRetryable = "failed_retryable"
	// ReviewRunEventFailedTerminal records a failure that cannot be retried for
	// this run.
	ReviewRunEventFailedTerminal = "failed_terminal"
	// ReviewRunEventCanceled records a user or service cancellation.
	ReviewRunEventCanceled = "canceled"
)

// AppendReviewRunEventInput identifies one safe lifecycle transition. The
// diagnostic is a closed, machine-readable code rather than error text, token,
// or engine output; the persisted JSON is therefore both bounded and safe to
// retain indefinitely.
type AppendReviewRunEventInput struct {
	RunID             string
	EventKind         string
	DiagnosticCode    string
	RetryAfterSeconds int
	OccurredAt        time.Time
}

// AppendReviewRunEventResult identifies the appended event. Repeating the
// exact same terminal outcome returns its original ID and sequence.
type AppendReviewRunEventResult struct {
	EventID  string
	Sequence int
	Created  bool
}

// AppendReviewRunEvent appends one failure or cancellation event to an
// existing run. Terminal outcomes are idempotent only when their exact bounded
// diagnostic matches; any competing terminal outcome fails closed. It does not
// create jobs, domain events, outbox records, assessments, or GitHub effects.
func (s *Store) AppendReviewRunEvent(ctx context.Context, input AppendReviewRunEventInput) (AppendReviewRunEventResult, error) {
	normalized, err := normalizeAppendReviewRunEventInput(input)
	if err != nil {
		return AppendReviewRunEventResult{}, err
	}

	var result AppendReviewRunEventResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		if err := requireReviewRunWithoutAssessment(ctx, conn, normalized.RunID); err != nil {
			return err
		}
		existing, found, err := loadTerminalReviewRunEvent(ctx, conn, normalized.RunID)
		if err != nil {
			return err
		}
		if found {
			if isTerminalReviewRunEvent(normalized.EventKind) &&
				existing.EventKind == normalized.EventKind && bytes.Equal(existing.PayloadJSON, normalized.PayloadJSON) {
				result = AppendReviewRunEventResult{EventID: existing.ID, Sequence: existing.Sequence}
				return nil
			}
			return fmt.Errorf("%w: run=%q", ErrReviewRunEventConflict, normalized.RunID)
		}

		sequence, err := nextReviewRunEventSequence(ctx, conn, normalized.RunID)
		if err != nil {
			return err
		}
		occurredAt := normalized.OccurredAt.UnixMicro()
		eventID := stableID("review-run-event", normalized.RunID, fmt.Sprintf("%d", sequence))
		if _, err := conn.ExecContext(ctx, `
INSERT INTO review_run_events(id, run_id, sequence, event_kind, payload_json, occurred_at_us, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			eventID, normalized.RunID, sequence, normalized.EventKind, normalized.PayloadJSON, occurredAt, occurredAt); err != nil {
			return fmt.Errorf("append review run event: %w", err)
		}
		result = AppendReviewRunEventResult{EventID: eventID, Sequence: sequence, Created: true}
		return nil
	})
	if err != nil {
		return AppendReviewRunEventResult{}, fmt.Errorf("append review run event: %w", err)
	}
	return result, nil
}

type normalizedAppendReviewRunEventInput struct {
	RunID       string
	EventKind   string
	PayloadJSON []byte
	OccurredAt  time.Time
}

func normalizeAppendReviewRunEventInput(input AppendReviewRunEventInput) (normalizedAppendReviewRunEventInput, error) {
	input.RunID = strings.TrimSpace(input.RunID)
	input.EventKind = strings.TrimSpace(input.EventKind)
	input.DiagnosticCode = strings.TrimSpace(input.DiagnosticCode)
	if input.RunID == "" || !validReviewRunFailureEvent(input.EventKind) ||
		!validReviewRunDiagnostic(input.EventKind, input.DiagnosticCode) ||
		input.RetryAfterSeconds < 0 || input.RetryAfterSeconds > 86_400 ||
		(input.EventKind != ReviewRunEventFailedRetryable && input.RetryAfterSeconds != 0) {
		return normalizedAppendReviewRunEventInput{}, errors.New("review run event input or diagnostic is invalid")
	}
	payload, err := json.Marshal(struct {
		Code              string `json:"code"`
		RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
	}{Code: input.DiagnosticCode, RetryAfterSeconds: input.RetryAfterSeconds})
	if err != nil {
		return normalizedAppendReviewRunEventInput{}, fmt.Errorf("encode review run diagnostic: %w", err)
	}
	if len(payload) > maxReviewRunDiagnosticPayloadBytes {
		return normalizedAppendReviewRunEventInput{}, errors.New("review run diagnostic payload exceeds bound")
	}
	if input.OccurredAt.IsZero() {
		input.OccurredAt = time.Now().UTC()
	} else {
		input.OccurredAt = input.OccurredAt.UTC()
	}
	if input.OccurredAt.UnixMicro() < 0 {
		return normalizedAppendReviewRunEventInput{}, errors.New("review run event time is invalid")
	}
	return normalizedAppendReviewRunEventInput{
		RunID: input.RunID, EventKind: input.EventKind, PayloadJSON: payload, OccurredAt: input.OccurredAt,
	}, nil
}

func validReviewRunFailureEvent(eventKind string) bool {
	return eventKind == ReviewRunEventFailedRetryable ||
		eventKind == ReviewRunEventFailedTerminal ||
		eventKind == ReviewRunEventCanceled
}

func isTerminalReviewRunEvent(eventKind string) bool {
	return eventKind == ReviewRunEventFailedTerminal || eventKind == ReviewRunEventCanceled
}

func validReviewRunDiagnostic(eventKind, code string) bool {
	var allowed []string
	switch eventKind {
	case ReviewRunEventFailedRetryable:
		allowed = []string{"transport_unavailable", "rate_limited", "timeout", "engine_exit", "internal_error"}
	case ReviewRunEventFailedTerminal:
		allowed = []string{"configuration_invalid", "input_invalid", "engine_protocol_invalid", "validation_failed", "stale_evidence"}
	case ReviewRunEventCanceled:
		allowed = []string{"canceled_by_request", "canceled_by_shutdown"}
	default:
		return false
	}
	for _, value := range allowed {
		if code == value {
			return true
		}
	}
	return false
}

func requireReviewRunWithoutAssessment(ctx context.Context, conn *sql.Conn, runID string) error {
	var hasRun, hasAssessment int
	if err := conn.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM review_runs WHERE id = ?),
       EXISTS(SELECT 1 FROM assessments WHERE run_id = ?)`, runID, runID).Scan(&hasRun, &hasAssessment); err != nil {
		return fmt.Errorf("load review run lifecycle: %w", err)
	}
	if hasRun == 0 {
		return errors.New("review run does not exist")
	}
	if hasAssessment != 0 {
		return ErrReviewRunSucceeded
	}
	return nil
}

type terminalReviewRunEvent struct {
	ID          string
	Sequence    int
	EventKind   string
	PayloadJSON []byte
}

func loadTerminalReviewRunEvent(ctx context.Context, conn *sql.Conn, runID string) (terminalReviewRunEvent, bool, error) {
	var value terminalReviewRunEvent
	err := conn.QueryRowContext(ctx, `
SELECT id, sequence, event_kind, payload_json
FROM review_run_events
WHERE run_id = ? AND event_kind IN ('failed_terminal', 'canceled')
ORDER BY sequence DESC
LIMIT 1`, runID).Scan(&value.ID, &value.Sequence, &value.EventKind, &value.PayloadJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return terminalReviewRunEvent{}, false, nil
	}
	if err != nil {
		return terminalReviewRunEvent{}, false, fmt.Errorf("load terminal review run event: %w", err)
	}
	return value, true, nil
}
