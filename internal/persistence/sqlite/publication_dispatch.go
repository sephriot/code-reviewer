package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxPublicationEffectIDBytes = 512

var (
	// ErrPublicationEffectNotFound means no immutable publication effect has
	// the requested ID.
	ErrPublicationEffectNotFound = errors.New("publication effect not found")
	// ErrPublicationEffectNotCurrent means an effect no longer has exact,
	// approved, current canonical evidence.
	ErrPublicationEffectNotCurrent = errors.New("publication effect is not current")
	// ErrPublicationAttemptConflict means stored attempt facts differ from the
	// single bounded simulated attempt protocol.
	ErrPublicationAttemptConflict = errors.New("publication attempt facts conflict")
)

// PublicationEffectTarget contains immutable effect facts only after its
// authorization chain and selected canonical revision both verify.
type PublicationEffectTarget struct {
	ID              string
	PublicationMode PublicationMode
}

// RecordSimulatedPublicationAttemptResult identifies a created or existing
// exactly-once simulated attempt. Disabled effects return an empty result.
type RecordSimulatedPublicationAttemptResult struct {
	AttemptID string
	Created   bool
}

// LoadCurrentPublicationEffect loads one effect only when its approved
// proposal, exact payload, and canonical evidence remain current. It never
// creates a publication attempt or performs GitHub traffic.
func (s *Store) LoadCurrentPublicationEffect(ctx context.Context, effectID string) (PublicationEffectTarget, error) {
	effectID, err := normalizePublicationEffectID(effectID)
	if err != nil {
		return PublicationEffectTarget{}, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return PublicationEffectTarget{}, fmt.Errorf("open publication effect read connection: %w", err)
	}
	defer conn.Close()
	effect, err := loadCurrentPublicationDispatchEffect(ctx, conn, effectID)
	if err != nil {
		return PublicationEffectTarget{}, err
	}
	return PublicationEffectTarget{ID: effect.ID, PublicationMode: effect.PublicationMode}, nil
}

// RecordSimulatedPublicationAttempt writes at most one exact simulated
// attempt after revalidating current canonical evidence. It is idempotent for
// a matching existing attempt; mismatched or additional attempts fail closed.
func (s *Store) RecordSimulatedPublicationAttempt(ctx context.Context, effectID string, attemptedAt time.Time) (RecordSimulatedPublicationAttemptResult, error) {
	effectID, err := normalizePublicationEffectID(effectID)
	if err != nil {
		return RecordSimulatedPublicationAttemptResult{}, err
	}
	attemptedAt, err = normalizePublicationAttemptTime(attemptedAt)
	if err != nil {
		return RecordSimulatedPublicationAttemptResult{}, err
	}

	var result RecordSimulatedPublicationAttemptResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		effect, err := loadCurrentPublicationDispatchEffect(ctx, conn, effectID)
		if err != nil {
			return err
		}
		existing, found, err := loadSimulatedPublicationAttempt(ctx, conn, effect.ID)
		if err != nil {
			return err
		}
		switch effect.PublicationMode {
		case PublicationModeDisabled:
			if found {
				return fmt.Errorf("%w: disabled effect=%q has attempt=%q", ErrPublicationAttemptConflict, effect.ID, existing.ID)
			}
			return nil
		case PublicationModeSimulated:
			if found {
				if !existing.matches(effect) {
					return fmt.Errorf("%w: effect=%q attempt=%q", ErrPublicationAttemptConflict, effect.ID, existing.ID)
				}
				result = RecordSimulatedPublicationAttemptResult{AttemptID: existing.ID}
				return nil
			}
		default:
			return fmt.Errorf("%w: unsupported mode=%q", ErrPublicationEffectNotCurrent, effect.PublicationMode)
		}

		attemptID := stableID("publication-attempt", effect.ID, "1")
		response := []byte(`{"simulated":true}`)
		if _, err := conn.ExecContext(ctx, `
INSERT INTO publication_attempts(
 id, effect_id, attempt_number, publication_mode, outcome, request_sha256,
 response_json, error_class, error_message, github_artifact_id,
 attempted_at_us, completed_at_us, created_at_us)
VALUES (?, ?, 1, 'simulated', 'simulated', ?, ?, NULL, NULL, NULL, ?, ?, ?)`,
			attemptID, effect.ID, effect.PayloadSHA256, response,
			attemptedAt.UnixMicro(), attemptedAt.UnixMicro(), attemptedAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert simulated publication attempt: %w", err)
		}
		result = RecordSimulatedPublicationAttemptResult{AttemptID: attemptID, Created: true}
		return nil
	})
	if err != nil {
		return RecordSimulatedPublicationAttemptResult{}, fmt.Errorf("record simulated publication attempt: %w", err)
	}
	return result, nil
}

type publicationDispatchEffect struct {
	ID                 string
	ProposalRevisionID string
	DecisionID         string
	ConnectionID       string
	RepositoryID       string
	PullRequestID      string
	RevisionID         string
	ObservationID      string
	EffectType         string
	PayloadJSON        []byte
	PayloadSHA256      string
	PublicationMode    PublicationMode
}

func loadCurrentPublicationDispatchEffect(ctx context.Context, conn *sql.Conn, effectID string) (publicationDispatchEffect, error) {
	effect, err := loadPublicationDispatchEffect(ctx, conn, effectID)
	if errors.Is(err, sql.ErrNoRows) {
		return publicationDispatchEffect{}, ErrPublicationEffectNotFound
	}
	if err != nil {
		return publicationDispatchEffect{}, fmt.Errorf("load publication effect: %w", err)
	}
	if err := validatePublicationDispatchEffect(ctx, conn, effect); err != nil {
		return publicationDispatchEffect{}, fmt.Errorf("%w: %v", ErrPublicationEffectNotCurrent, err)
	}
	current, err := loadCurrentCanonicalReviewTarget(ctx, conn, effect.ConnectionID, effect.PullRequestID)
	if err != nil {
		return publicationDispatchEffect{}, fmt.Errorf("%w: current canonical evidence", ErrPublicationEffectNotCurrent)
	}
	if effect.ConnectionID != current.ConnectionID || effect.RepositoryID != current.RepositoryID ||
		effect.PullRequestID != current.PullRequestID || effect.RevisionID != current.RevisionID ||
		effect.ObservationID != current.ObservationID {
		return publicationDispatchEffect{}, fmt.Errorf("%w: effect evidence differs from current selection", ErrPublicationEffectNotCurrent)
	}
	return effect, nil
}

func loadPublicationDispatchEffect(ctx context.Context, conn *sql.Conn, effectID string) (publicationDispatchEffect, error) {
	var effect publicationDispatchEffect
	err := conn.QueryRowContext(ctx, `
SELECT id, proposal_revision_id, authorization_decision_id,
       connection_id, repository_id, pull_request_id, revision_id, observation_id,
       effect_type, payload_json, payload_sha256, publication_mode_at_authorization
FROM publication_effects
WHERE id = ?`, effectID).Scan(
		&effect.ID, &effect.ProposalRevisionID, &effect.DecisionID,
		&effect.ConnectionID, &effect.RepositoryID, &effect.PullRequestID, &effect.RevisionID, &effect.ObservationID,
		&effect.EffectType, &effect.PayloadJSON, &effect.PayloadSHA256, &effect.PublicationMode,
	)
	return effect, err
}

func validatePublicationDispatchEffect(ctx context.Context, conn *sql.Conn, effect publicationDispatchEffect) error {
	if effect.ID == "" || effect.ProposalRevisionID == "" || effect.DecisionID == "" ||
		effect.ConnectionID == "" || effect.RepositoryID == "" || effect.PullRequestID == "" ||
		effect.RevisionID == "" || effect.ObservationID == "" || !validLowerHexDigest(effect.PayloadSHA256) ||
		(effect.PublicationMode != PublicationModeDisabled && effect.PublicationMode != PublicationModeSimulated && effect.PublicationMode != PublicationModeEnabled) {
		return errors.New("stored publication effect facts are invalid")
	}
	authorization, err := loadApprovedPublicationAuthorization(ctx, conn, effect.ProposalRevisionID)
	if err != nil {
		return errors.New("approved publication authorization is absent")
	}
	payload, payloadSHA256, err := authorization.payload()
	if err != nil {
		return errors.New("approved publication payload is invalid")
	}
	effectType, err := publicationEffectType(authorization.ProposalKind)
	if err != nil {
		return errors.New("approved publication effect type is invalid")
	}
	if authorization.DecisionID != effect.DecisionID || !authorization.matchesTarget(CanonicalReviewTarget{
		ConnectionID: effect.ConnectionID, RepositoryID: effect.RepositoryID, PullRequestID: effect.PullRequestID,
		RevisionID: effect.RevisionID, ObservationID: effect.ObservationID,
	}) || effect.EffectType != effectType || !bytes.Equal(effect.PayloadJSON, payload) || effect.PayloadSHA256 != payloadSHA256 {
		return errors.New("stored publication effect differs from approved authorization")
	}
	return nil
}

type storedSimulatedPublicationAttempt struct {
	ID              string
	AttemptNumber   int
	PublicationMode string
	Outcome         string
	RequestSHA256   string
	ResponseJSON    []byte
	ErrorClass      sql.NullString
	ErrorMessage    sql.NullString
	GitHubArtifact  sql.NullString
}

func loadSimulatedPublicationAttempt(ctx context.Context, conn *sql.Conn, effectID string) (storedSimulatedPublicationAttempt, bool, error) {
	rows, err := conn.QueryContext(ctx, `
SELECT id, attempt_number, publication_mode, outcome, request_sha256, response_json,
       error_class, error_message, github_artifact_id
FROM publication_attempts WHERE effect_id = ? ORDER BY attempt_number, id`, effectID)
	if err != nil {
		return storedSimulatedPublicationAttempt{}, false, fmt.Errorf("load publication attempts: %w", err)
	}
	defer rows.Close()
	var attempt storedSimulatedPublicationAttempt
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return storedSimulatedPublicationAttempt{}, false, fmt.Errorf("iterate publication attempts: %w", err)
		}
		return storedSimulatedPublicationAttempt{}, false, nil
	}
	if err := rows.Scan(&attempt.ID, &attempt.AttemptNumber, &attempt.PublicationMode, &attempt.Outcome,
		&attempt.RequestSHA256, &attempt.ResponseJSON, &attempt.ErrorClass, &attempt.ErrorMessage, &attempt.GitHubArtifact); err != nil {
		return storedSimulatedPublicationAttempt{}, false, fmt.Errorf("scan publication attempt: %w", err)
	}
	if rows.Next() {
		return storedSimulatedPublicationAttempt{}, false, fmt.Errorf("%w: effect=%q has multiple attempts", ErrPublicationAttemptConflict, effectID)
	}
	if err := rows.Err(); err != nil {
		return storedSimulatedPublicationAttempt{}, false, fmt.Errorf("iterate publication attempts: %w", err)
	}
	return attempt, true, nil
}

func (attempt storedSimulatedPublicationAttempt) matches(effect publicationDispatchEffect) bool {
	return attempt.ID == stableID("publication-attempt", effect.ID, "1") && attempt.AttemptNumber == 1 &&
		attempt.PublicationMode == string(PublicationModeSimulated) && attempt.Outcome == "simulated" &&
		attempt.RequestSHA256 == effect.PayloadSHA256 && bytes.Equal(attempt.ResponseJSON, []byte(`{"simulated":true}`)) &&
		!attempt.ErrorClass.Valid && !attempt.ErrorMessage.Valid && !attempt.GitHubArtifact.Valid
}

func normalizePublicationEffectID(effectID string) (string, error) {
	effectID = strings.TrimSpace(effectID)
	if effectID == "" || len(effectID) > maxPublicationEffectIDBytes {
		return "", errors.New("publication effect ID is invalid")
	}
	return effectID, nil
}

func normalizePublicationAttemptTime(value time.Time) (time.Time, error) {
	if value.IsZero() {
		value = time.Now().UTC()
	} else {
		value = value.UTC()
	}
	if value.UnixMicro() < 0 {
		return time.Time{}, errors.New("publication attempt time is invalid")
	}
	return value, nil
}
