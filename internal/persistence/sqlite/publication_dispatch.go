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

const (
	maxPublicationEffectIDBytes          = 512
	maxPublicationAttemptResponseBytes   = 64 * 1024
	maxPublicationAttemptErrorBytes      = 4 * 1024
	maxPublicationAttemptArtifactIDBytes = 512
)

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
	// ErrPublicationEffectNotDispatchable means an enabled effect cannot safely
	// start another outbound request.
	ErrPublicationEffectNotDispatchable = errors.New("publication effect is not dispatchable")
	// ErrPublicationUncertaintyNotResolvable means the effect has no unresolved
	// enabled attempt with an uncertain delivery outcome.
	ErrPublicationUncertaintyNotResolvable = errors.New("publication uncertainty is not resolvable")
	// ErrPublicationUncertaintyResolutionConflict means an immutable resolution
	// already binds the effect or idempotency key to different facts.
	ErrPublicationUncertaintyResolutionConflict = errors.New("publication uncertainty resolution facts conflict")
)

// PublicationEffectTarget contains immutable effect facts only after its
// authorization chain and selected canonical revision both verify.
type PublicationEffectTarget struct {
	ID                string
	PublicationMode   PublicationMode
	Owner             string
	Repository        string
	PullRequestNumber int
	EffectType        string
	PayloadJSON       []byte
	PayloadSHA256     string
}

// ClaimEnabledPublicationAttemptResult identifies the one durable pre-send
// claim allowed for an enabled effect. Existing claims are returned unchanged
// so callers can convert an interrupted request into uncertainty instead of
// sending the same effect again.
type ClaimEnabledPublicationAttemptResult struct {
	Effect    PublicationEffectTarget
	ClaimID   string
	ClaimedAt time.Time
	Created   bool
}

// PublicationAttemptOutcome is the terminal local classification of one
// enabled request. Any result after a request may have been transmitted is
// uncertain rather than retryable.
type PublicationAttemptOutcome string

const (
	PublicationAttemptSucceeded      PublicationAttemptOutcome = "succeeded"
	PublicationAttemptFailedTerminal PublicationAttemptOutcome = "failed_terminal"
	PublicationAttemptUncertain      PublicationAttemptOutcome = "uncertain"
)

// RecordEnabledPublicationAttemptInput records one terminal outcome for the
// existing pre-send claim. Response data must be bounded JSON metadata, never
// raw provider bodies or credentials.
type RecordEnabledPublicationAttemptInput struct {
	EffectID         string
	Outcome          PublicationAttemptOutcome
	ResponseJSON     []byte
	ErrorClass       string
	ErrorMessage     string
	GitHubArtifactID string
	CompletedAt      time.Time
}

// RecordEnabledPublicationAttemptResult identifies one immutable recorded
// outcome. Replays with identical facts return the existing record.
type RecordEnabledPublicationAttemptResult struct {
	AttemptID string
	Created   bool
}

// PublicationEffectStatus is a safe read model for one immutable effect. It
// omits rendered payloads, provider responses, errors, and actor details.
type PublicationEffectStatus struct {
	EffectID        string
	PublicationMode PublicationMode
	Attempt         *PublicationAttemptStatus
	Resolution      *PublicationUncertaintyResolutionStatus
}

// PublicationAttemptStatus identifies the latest local delivery result.
type PublicationAttemptStatus struct {
	AttemptID       string
	PublicationMode PublicationMode
	Outcome         string
	CompletedAt     time.Time
}

// PublicationUncertaintyResolutionStatus identifies an immutable human
// terminal classification without exposing the operator's reason.
type PublicationUncertaintyResolutionStatus struct {
	ResolutionID string
	Resolution   PublicationUncertaintyResolution
	ResolvedAt   time.Time
}

// PublicationUncertaintyResolution is an operator's terminal classification
// of an enabled delivery whose external result cannot be safely inferred.
type PublicationUncertaintyResolution string

const (
	// PublicationUncertaintyExternallyCompleted records an operator's verified
	// finding that GitHub received the effect.
	PublicationUncertaintyExternallyCompleted PublicationUncertaintyResolution = "externally_completed"
	// PublicationUncertaintyAbandoned records an operator's decision not to
	// pursue an uncertain effect further.
	PublicationUncertaintyAbandoned PublicationUncertaintyResolution = "abandoned"
)

// ResolvePublicationUncertaintyInput records one human terminal resolution.
// It never requeues or sends a GitHub request.
type ResolvePublicationUncertaintyInput struct {
	EffectID       string
	Resolution     PublicationUncertaintyResolution
	ActorID        string
	IdempotencyKey string
	Reason         string
	ResolvedAt     time.Time
}

// ResolvePublicationUncertaintyResult identifies one immutable resolution.
type ResolvePublicationUncertaintyResult struct {
	ResolutionID string
	Created      bool
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
	target, err := loadPublicationEffectTarget(ctx, conn, effect)
	if err != nil {
		return PublicationEffectTarget{}, err
	}
	return target, nil
}

// PublicationEffectStatus loads immutable delivery state for local operator
// inspection. It accepts stale effects because uncertainty must remain
// resolvable after newer canonical evidence arrives.
func (s *Store) PublicationEffectStatus(ctx context.Context, effectID string) (PublicationEffectStatus, error) {
	effectID, err := normalizePublicationEffectID(effectID)
	if err != nil {
		return PublicationEffectStatus{}, err
	}
	var status PublicationEffectStatus
	var attemptID, attemptMode, attemptOutcome, resolutionID, resolution string
	var completedAtUS, resolvedAtUS sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
SELECT effect.id, effect.publication_mode_at_authorization,
       COALESCE(attempt.id, ''), COALESCE(attempt.publication_mode, ''), COALESCE(attempt.outcome, ''), attempt.completed_at_us,
       COALESCE(resolution.id, ''), COALESCE(resolution.resolution, ''), resolution.resolved_at_us
FROM publication_effects AS effect
LEFT JOIN publication_attempts AS attempt ON attempt.id = (
  SELECT candidate.id FROM publication_attempts AS candidate
  WHERE candidate.effect_id = effect.id
  ORDER BY candidate.attempt_number DESC, candidate.id DESC LIMIT 1
)
LEFT JOIN publication_uncertainty_resolutions AS resolution ON resolution.effect_id = effect.id
WHERE effect.id = ?`, effectID).Scan(
		&status.EffectID, &status.PublicationMode,
		&attemptID, &attemptMode, &attemptOutcome, &completedAtUS,
		&resolutionID, &resolution, &resolvedAtUS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PublicationEffectStatus{}, ErrPublicationEffectNotFound
	}
	if err != nil {
		return PublicationEffectStatus{}, fmt.Errorf("load publication effect status: %w", err)
	}
	if attemptID != "" {
		if !completedAtUS.Valid {
			return PublicationEffectStatus{}, errors.New("stored publication attempt status is invalid")
		}
		status.Attempt = &PublicationAttemptStatus{
			AttemptID: attemptID, PublicationMode: PublicationMode(attemptMode),
			Outcome: attemptOutcome, CompletedAt: time.UnixMicro(completedAtUS.Int64).UTC(),
		}
	}
	if resolutionID != "" {
		if !resolvedAtUS.Valid {
			return PublicationEffectStatus{}, errors.New("stored publication uncertainty resolution is invalid")
		}
		status.Resolution = &PublicationUncertaintyResolutionStatus{
			ResolutionID: resolutionID, Resolution: PublicationUncertaintyResolution(resolution),
			ResolvedAt: time.UnixMicro(resolvedAtUS.Int64).UTC(),
		}
	}
	return status, nil
}

// ClaimEnabledPublicationAttempt writes the immutable pre-send claim for a
// current enabled effect. It performs no network activity and intentionally
// permits only one claim: callers must treat a recovered claim as uncertain.
func (s *Store) ClaimEnabledPublicationAttempt(ctx context.Context, effectID string, claimedAt time.Time) (ClaimEnabledPublicationAttemptResult, error) {
	effectID, err := normalizePublicationEffectID(effectID)
	if err != nil {
		return ClaimEnabledPublicationAttemptResult{}, err
	}
	claimedAt, err = normalizePublicationAttemptTime(claimedAt)
	if err != nil {
		return ClaimEnabledPublicationAttemptResult{}, err
	}

	var result ClaimEnabledPublicationAttemptResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		effect, err := loadCurrentPublicationDispatchEffect(ctx, conn, effectID)
		if err != nil {
			return err
		}
		if effect.PublicationMode != PublicationModeEnabled {
			return fmt.Errorf("%w: effect=%q mode=%q", ErrPublicationEffectNotDispatchable, effect.ID, effect.PublicationMode)
		}
		target, err := loadPublicationEffectTarget(ctx, conn, effect)
		if err != nil {
			return err
		}
		if attemptCount, err := countPublicationAttempts(ctx, conn, effect.ID); err != nil {
			return err
		} else if attemptCount != 0 {
			return fmt.Errorf("%w: effect=%q already has attempt", ErrPublicationEffectNotDispatchable, effect.ID)
		}
		claim, found, err := loadPublicationDispatchClaim(ctx, conn, effect.ID)
		if err != nil {
			return err
		}
		if found {
			result = ClaimEnabledPublicationAttemptResult{Effect: target, ClaimID: claim.ID, ClaimedAt: time.UnixMicro(claim.ClaimedAtUS).UTC()}
			return nil
		}
		claimID := stableID("publication-dispatch-claim", effect.ID, "1")
		if _, err := conn.ExecContext(ctx, `
INSERT INTO publication_dispatch_claims(id, effect_id, attempt_number, request_sha256, claimed_at_us)
VALUES (?, ?, 1, ?, ?)`, claimID, effect.ID, effect.PayloadSHA256, claimedAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert publication dispatch claim: %w", err)
		}
		result = ClaimEnabledPublicationAttemptResult{Effect: target, ClaimID: claimID, ClaimedAt: claimedAt, Created: true}
		return nil
	})
	if err != nil {
		return ClaimEnabledPublicationAttemptResult{}, fmt.Errorf("claim enabled publication attempt: %w", err)
	}
	return result, nil
}

// RecordEnabledPublicationAttempt records a terminal outcome only for the
// existing current enabled claim. It does not send a request or create another
// claim, so a crash path cannot accidentally produce a duplicate GitHub write.
func (s *Store) RecordEnabledPublicationAttempt(ctx context.Context, input RecordEnabledPublicationAttemptInput) (RecordEnabledPublicationAttemptResult, error) {
	normalized, err := normalizeRecordEnabledPublicationAttemptInput(input)
	if err != nil {
		return RecordEnabledPublicationAttemptResult{}, err
	}
	var result RecordEnabledPublicationAttemptResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		effect, err := loadCurrentPublicationDispatchEffect(ctx, conn, normalized.EffectID)
		if err != nil {
			return err
		}
		if effect.PublicationMode != PublicationModeEnabled {
			return fmt.Errorf("%w: effect=%q mode=%q", ErrPublicationEffectNotDispatchable, effect.ID, effect.PublicationMode)
		}
		claim, found, err := loadPublicationDispatchClaim(ctx, conn, effect.ID)
		if err != nil {
			return err
		}
		if !found || claim.ClaimedAtUS > normalized.CompletedAt.UnixMicro() {
			return fmt.Errorf("%w: effect=%q has no valid pre-send claim", ErrPublicationEffectNotDispatchable, effect.ID)
		}
		existing, found, err := loadEnabledPublicationAttempt(ctx, conn, effect.ID)
		if err != nil {
			return err
		}
		if found {
			if !existing.matches(effect, claim, normalized) {
				return fmt.Errorf("%w: effect=%q attempt=%q", ErrPublicationAttemptConflict, effect.ID, existing.ID)
			}
			result = RecordEnabledPublicationAttemptResult{AttemptID: existing.ID}
			return nil
		}
		attemptID := stableID("publication-attempt", effect.ID, "1")
		if _, err := conn.ExecContext(ctx, `
INSERT INTO publication_attempts(
 id, effect_id, attempt_number, publication_mode, outcome, request_sha256,
 response_json, error_class, error_message, github_artifact_id,
 attempted_at_us, completed_at_us, created_at_us)
VALUES (?, ?, 1, 'enabled', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			attemptID, effect.ID, normalized.Outcome, effect.PayloadSHA256,
			normalized.ResponseJSON, nullableString(normalized.ErrorClass), nullableString(normalized.ErrorMessage),
			nullableString(normalized.GitHubArtifactID), claim.ClaimedAtUS, normalized.CompletedAt.UnixMicro(), normalized.CompletedAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert enabled publication attempt: %w", err)
		}
		result = RecordEnabledPublicationAttemptResult{AttemptID: attemptID, Created: true}
		return nil
	})
	if err != nil {
		return RecordEnabledPublicationAttemptResult{}, fmt.Errorf("record enabled publication attempt: %w", err)
	}
	return result, nil
}

// ResolvePublicationUncertainty records a human-only terminal resolution for
// an uncertain enabled delivery. It deliberately accepts stale effects: an
// operator must still be able to close a historical uncertainty after a PR
// advances. No code path here can create a new claim, job, or outbound call.
func (s *Store) ResolvePublicationUncertainty(ctx context.Context, input ResolvePublicationUncertaintyInput) (ResolvePublicationUncertaintyResult, error) {
	normalized, err := normalizeResolvePublicationUncertaintyInput(input)
	if err != nil {
		return ResolvePublicationUncertaintyResult{}, err
	}
	var result ResolvePublicationUncertaintyResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		effect, err := loadPublicationDispatchEffect(ctx, conn, normalized.EffectID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPublicationEffectNotFound
		}
		if err != nil {
			return fmt.Errorf("load publication effect: %w", err)
		}
		if effect.PublicationMode != PublicationModeEnabled {
			return fmt.Errorf("%w: effect=%q mode=%q", ErrPublicationUncertaintyNotResolvable, effect.ID, effect.PublicationMode)
		}
		attempt, found, err := loadEnabledPublicationAttempt(ctx, conn, effect.ID)
		if err != nil {
			return err
		}
		if !found || attempt.Outcome != string(PublicationAttemptUncertain) {
			return fmt.Errorf("%w: effect=%q", ErrPublicationUncertaintyNotResolvable, effect.ID)
		}
		existing, found, err := loadPublicationUncertaintyResolution(ctx, conn, normalized.EffectID, normalized.IdempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if !existing.matches(effect.ID, attempt.ID, normalized) {
				return fmt.Errorf("%w: effect=%q", ErrPublicationUncertaintyResolutionConflict, effect.ID)
			}
			result = ResolvePublicationUncertaintyResult{ResolutionID: existing.ID}
			return nil
		}
		resolutionID := stableID("publication-uncertainty-resolution", effect.ID, normalized.IdempotencyKey)
		if _, err := conn.ExecContext(ctx, `
INSERT INTO publication_uncertainty_resolutions(
 id, effect_id, attempt_id, resolution, actor_id, idempotency_key, reason, resolved_at_us, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			resolutionID, effect.ID, attempt.ID, normalized.Resolution, normalized.ActorID,
			normalized.IdempotencyKey, normalized.Reason, normalized.ResolvedAt.UnixMicro(), normalized.ResolvedAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert publication uncertainty resolution: %w", err)
		}
		result = ResolvePublicationUncertaintyResult{ResolutionID: resolutionID, Created: true}
		return nil
	})
	if err != nil {
		return ResolvePublicationUncertaintyResult{}, fmt.Errorf("resolve publication uncertainty: %w", err)
	}
	return result, nil
}

type normalizedRecordEnabledPublicationAttemptInput struct {
	EffectID         string
	Outcome          PublicationAttemptOutcome
	ResponseJSON     []byte
	ErrorClass       string
	ErrorMessage     string
	GitHubArtifactID string
	CompletedAt      time.Time
}

func normalizeRecordEnabledPublicationAttemptInput(input RecordEnabledPublicationAttemptInput) (normalizedRecordEnabledPublicationAttemptInput, error) {
	effectID, err := normalizePublicationEffectID(input.EffectID)
	if err != nil {
		return normalizedRecordEnabledPublicationAttemptInput{}, err
	}
	if input.Outcome != PublicationAttemptSucceeded && input.Outcome != PublicationAttemptFailedTerminal && input.Outcome != PublicationAttemptUncertain {
		return normalizedRecordEnabledPublicationAttemptInput{}, errors.New("enabled publication attempt outcome is invalid")
	}
	response, err := normalizeBoundedJSONObject(input.ResponseJSON, maxPublicationAttemptResponseBytes)
	if err != nil {
		return normalizedRecordEnabledPublicationAttemptInput{}, fmt.Errorf("enabled publication attempt response: %w", err)
	}
	input.ErrorClass = strings.TrimSpace(input.ErrorClass)
	input.ErrorMessage = strings.TrimSpace(input.ErrorMessage)
	input.GitHubArtifactID = strings.TrimSpace(input.GitHubArtifactID)
	if len(input.ErrorClass) > maxPublicationAttemptErrorBytes || len(input.ErrorMessage) > maxPublicationAttemptErrorBytes || len(input.GitHubArtifactID) > maxPublicationAttemptArtifactIDBytes {
		return normalizedRecordEnabledPublicationAttemptInput{}, errors.New("enabled publication attempt metadata exceeds maximum size")
	}
	if input.Outcome == PublicationAttemptSucceeded && (input.ErrorClass != "" || input.ErrorMessage != "" || input.GitHubArtifactID == "") {
		return normalizedRecordEnabledPublicationAttemptInput{}, errors.New("successful enabled publication attempt metadata is invalid")
	}
	if input.Outcome != PublicationAttemptSucceeded && (input.ErrorClass == "" || input.ErrorMessage == "") {
		return normalizedRecordEnabledPublicationAttemptInput{}, errors.New("failed enabled publication attempt metadata is invalid")
	}
	completedAt, err := normalizePublicationAttemptTime(input.CompletedAt)
	if err != nil {
		return normalizedRecordEnabledPublicationAttemptInput{}, err
	}
	return normalizedRecordEnabledPublicationAttemptInput{
		EffectID: effectID, Outcome: input.Outcome, ResponseJSON: response,
		ErrorClass: input.ErrorClass, ErrorMessage: input.ErrorMessage,
		GitHubArtifactID: input.GitHubArtifactID, CompletedAt: completedAt,
	}, nil
}

type normalizedResolvePublicationUncertaintyInput struct {
	ResolvePublicationUncertaintyInput
}

func normalizeResolvePublicationUncertaintyInput(input ResolvePublicationUncertaintyInput) (normalizedResolvePublicationUncertaintyInput, error) {
	effectID, err := normalizePublicationEffectID(input.EffectID)
	if err != nil {
		return normalizedResolvePublicationUncertaintyInput{}, err
	}
	input.EffectID = effectID
	input.ActorID = strings.TrimSpace(input.ActorID)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.Reason = strings.ReplaceAll(strings.ReplaceAll(input.Reason, "\r\n", "\n"), "\r", "\n")
	if (input.Resolution != PublicationUncertaintyExternallyCompleted && input.Resolution != PublicationUncertaintyAbandoned) ||
		input.ActorID == "" || len(input.ActorID) > maxPublicationEffectIDBytes ||
		input.IdempotencyKey == "" || len(input.IdempotencyKey) > maxPublicationEffectIDBytes ||
		len(input.Reason) > maxProposalDecisionReasonBytes {
		return normalizedResolvePublicationUncertaintyInput{}, errors.New("publication uncertainty resolution input is invalid")
	}
	input.ResolvedAt, err = normalizePublicationAttemptTime(input.ResolvedAt)
	if err != nil {
		return normalizedResolvePublicationUncertaintyInput{}, err
	}
	return normalizedResolvePublicationUncertaintyInput{ResolvePublicationUncertaintyInput: input}, nil
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

func loadPublicationEffectTarget(ctx context.Context, conn *sql.Conn, effect publicationDispatchEffect) (PublicationEffectTarget, error) {
	var target PublicationEffectTarget
	var payload []byte
	err := conn.QueryRowContext(ctx, `
SELECT repository.owner_login, repository.name, pull_request.number
FROM repositories AS repository
JOIN pull_requests AS pull_request
  ON pull_request.id = ? AND pull_request.repository_id = repository.id
WHERE repository.id = ?`, effect.PullRequestID, effect.RepositoryID).Scan(
		&target.Owner, &target.Repository, &target.PullRequestNumber,
	)
	if err != nil {
		return PublicationEffectTarget{}, fmt.Errorf("load publication target coordinates: %w", err)
	}
	if strings.TrimSpace(target.Owner) == "" || strings.TrimSpace(target.Repository) == "" || target.PullRequestNumber < 1 {
		return PublicationEffectTarget{}, errors.New("stored publication target coordinates are invalid")
	}
	payload = append(payload, effect.PayloadJSON...)
	return PublicationEffectTarget{
		ID: effect.ID, PublicationMode: effect.PublicationMode,
		Owner: target.Owner, Repository: target.Repository, PullRequestNumber: target.PullRequestNumber,
		EffectType: effect.EffectType, PayloadJSON: payload, PayloadSHA256: effect.PayloadSHA256,
	}, nil
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

type publicationDispatchClaim struct {
	ID          string
	ClaimedAtUS int64
}

type storedEnabledPublicationAttempt struct {
	ID               string
	AttemptNumber    int
	PublicationMode  string
	Outcome          string
	RequestSHA256    string
	ResponseJSON     []byte
	ErrorClass       sql.NullString
	ErrorMessage     sql.NullString
	GitHubArtifactID sql.NullString
	AttemptedAtUS    int64
	CompletedAtUS    int64
}

type storedPublicationUncertaintyResolution struct {
	ID             string
	EffectID       string
	AttemptID      string
	Resolution     PublicationUncertaintyResolution
	ActorID        string
	IdempotencyKey string
	Reason         string
}

func loadPublicationUncertaintyResolution(ctx context.Context, conn *sql.Conn, effectID, idempotencyKey string) (storedPublicationUncertaintyResolution, bool, error) {
	var resolution storedPublicationUncertaintyResolution
	err := conn.QueryRowContext(ctx, `
SELECT id, effect_id, attempt_id, resolution, actor_id, idempotency_key, reason
FROM publication_uncertainty_resolutions
WHERE effect_id = ? OR idempotency_key = ?
ORDER BY resolved_at_us, id
LIMIT 1`, effectID, idempotencyKey).Scan(
		&resolution.ID, &resolution.EffectID, &resolution.AttemptID, &resolution.Resolution,
		&resolution.ActorID, &resolution.IdempotencyKey, &resolution.Reason,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedPublicationUncertaintyResolution{}, false, nil
	}
	if err != nil {
		return storedPublicationUncertaintyResolution{}, false, fmt.Errorf("load publication uncertainty resolution: %w", err)
	}
	return resolution, true, nil
}

func (resolution storedPublicationUncertaintyResolution) matches(effectID, attemptID string, input normalizedResolvePublicationUncertaintyInput) bool {
	return resolution.ID == stableID("publication-uncertainty-resolution", effectID, input.IdempotencyKey) &&
		resolution.EffectID == effectID && resolution.AttemptID == attemptID &&
		resolution.Resolution == input.Resolution && resolution.ActorID == input.ActorID &&
		resolution.IdempotencyKey == input.IdempotencyKey && resolution.Reason == input.Reason
}

func loadEnabledPublicationAttempt(ctx context.Context, conn *sql.Conn, effectID string) (storedEnabledPublicationAttempt, bool, error) {
	var attempt storedEnabledPublicationAttempt
	err := conn.QueryRowContext(ctx, `
SELECT id, attempt_number, publication_mode, outcome, request_sha256, response_json,
       error_class, error_message, github_artifact_id, attempted_at_us, completed_at_us
FROM publication_attempts WHERE effect_id = ?`, effectID).Scan(
		&attempt.ID, &attempt.AttemptNumber, &attempt.PublicationMode, &attempt.Outcome, &attempt.RequestSHA256,
		&attempt.ResponseJSON, &attempt.ErrorClass, &attempt.ErrorMessage, &attempt.GitHubArtifactID,
		&attempt.AttemptedAtUS, &attempt.CompletedAtUS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedEnabledPublicationAttempt{}, false, nil
	}
	if err != nil {
		return storedEnabledPublicationAttempt{}, false, fmt.Errorf("load enabled publication attempt: %w", err)
	}
	return attempt, true, nil
}

func (attempt storedEnabledPublicationAttempt) matches(effect publicationDispatchEffect, claim publicationDispatchClaim, input normalizedRecordEnabledPublicationAttemptInput) bool {
	return attempt.ID == stableID("publication-attempt", effect.ID, "1") && attempt.AttemptNumber == 1 &&
		attempt.PublicationMode == string(PublicationModeEnabled) && attempt.Outcome == string(input.Outcome) &&
		attempt.RequestSHA256 == effect.PayloadSHA256 && bytes.Equal(attempt.ResponseJSON, input.ResponseJSON) &&
		matchesNullableString(attempt.ErrorClass, input.ErrorClass) && matchesNullableString(attempt.ErrorMessage, input.ErrorMessage) &&
		matchesNullableString(attempt.GitHubArtifactID, input.GitHubArtifactID) && attempt.AttemptedAtUS == claim.ClaimedAtUS &&
		attempt.CompletedAtUS == input.CompletedAt.UnixMicro()
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func matchesNullableString(stored sql.NullString, value string) bool {
	return (value == "" && !stored.Valid) || (value != "" && stored.Valid && stored.String == value)
}

func loadPublicationDispatchClaim(ctx context.Context, conn *sql.Conn, effectID string) (publicationDispatchClaim, bool, error) {
	var claim publicationDispatchClaim
	err := conn.QueryRowContext(ctx, `
SELECT id, claimed_at_us FROM publication_dispatch_claims WHERE effect_id = ?`, effectID).Scan(&claim.ID, &claim.ClaimedAtUS)
	if errors.Is(err, sql.ErrNoRows) {
		return publicationDispatchClaim{}, false, nil
	}
	if err != nil {
		return publicationDispatchClaim{}, false, fmt.Errorf("load publication dispatch claim: %w", err)
	}
	return claim, true, nil
}

func countPublicationAttempts(ctx context.Context, conn *sql.Conn, effectID string) (int, error) {
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM publication_attempts WHERE effect_id = ?`, effectID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count publication attempts: %w", err)
	}
	return count, nil
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
