package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxProposalDecisionReasonBytes = 16 * 1024

var (
	// ErrProposalNotFound means no immutable proposal or proposal revision
	// exists for a requested human edit or decision.
	ErrProposalNotFound = errors.New("proposal not found")
	// ErrProposalDecisionConflict means an idempotency key or proposal revision
	// is already bound to a different immutable decision.
	ErrProposalDecisionConflict = errors.New("proposal decision facts conflict")
)

// CreateHumanProposalRevisionInput supplies one append-only human edit. A
// proposal has no mutable draft row: every successful edit receives the next
// revision number and remains pinned to its original canonical evidence.
type CreateHumanProposalRevisionInput struct {
	ProposalID         string
	Body               string
	InlineCommentsJSON []byte
	EditedAt           time.Time
}

// CreateHumanProposalRevisionResult identifies the appended human revision.
type CreateHumanProposalRevisionResult struct {
	ProposalRevisionID string
	RevisionNumber     int
}

// CreateHumanProposalRevision appends a normalized human revision only when
// its proposal still matches currently selected verified canonical evidence.
// It creates no decision, publication effect, job, event, or outbox work.
func (s *Store) CreateHumanProposalRevision(ctx context.Context, input CreateHumanProposalRevisionInput) (CreateHumanProposalRevisionResult, error) {
	normalized, err := normalizeCreateHumanProposalRevisionInput(input)
	if err != nil {
		return CreateHumanProposalRevisionResult{}, err
	}

	var result CreateHumanProposalRevisionResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		proposal, err := loadStoredProposal(ctx, conn, normalized.ProposalID)
		if err != nil {
			return err
		}
		target, err := loadCurrentCanonicalReviewTarget(ctx, conn, proposal.ConnectionID, proposal.PullRequestID)
		if err != nil {
			return err
		}
		if !proposal.matchesCurrentTarget(target) {
			return errors.New("proposal no longer matches current canonical evidence")
		}
		nextNumber, err := nextProposalRevisionNumber(ctx, conn, proposal.ProposalID)
		if err != nil {
			return err
		}
		id := stableID("proposal-revision", proposal.ProposalID, fmt.Sprintf("%d", nextNumber))
		if _, err := conn.ExecContext(ctx, `
INSERT INTO proposal_revisions(
 id, proposal_id, policy_evaluation_id, assessment_id, run_id, intent_id,
 pull_request_id, revision_id, observation_id, revision_number, editor_kind,
 body, inline_comments_json, content_sha256, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'human', ?, ?, ?, ?)`,
			id, proposal.ProposalID, proposal.PolicyEvaluationID, proposal.AssessmentID,
			proposal.RunID, proposal.IntentID, proposal.PullRequestID, proposal.RevisionID,
			proposal.ObservationID, nextNumber, normalized.Body, normalized.InlineCommentsJSON,
			normalized.ContentSHA256, normalized.EditedAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert human proposal revision: %w", err)
		}
		result = CreateHumanProposalRevisionResult{ProposalRevisionID: id, RevisionNumber: nextNumber}
		return nil
	})
	if err != nil {
		return CreateHumanProposalRevisionResult{}, fmt.Errorf("create human proposal revision: %w", err)
	}
	return result, nil
}

type normalizedCreateHumanProposalRevisionInput struct {
	CreateHumanProposalRevisionInput
	ContentSHA256 string
}

func normalizeCreateHumanProposalRevisionInput(input CreateHumanProposalRevisionInput) (normalizedCreateHumanProposalRevisionInput, error) {
	input.ProposalID = strings.TrimSpace(input.ProposalID)
	if input.ProposalID == "" || len(input.ProposalID) > 512 {
		return normalizedCreateHumanProposalRevisionInput{}, errors.New("proposal revision input is invalid")
	}
	inline, err := normalizeBoundedJSONArray(input.InlineCommentsJSON, maxProposalCommentsBytes)
	if err != nil {
		return normalizedCreateHumanProposalRevisionInput{}, fmt.Errorf("proposal inline comments: %w", err)
	}
	body, contentSHA256, err := normalizeProposalContent(input.Body, inline)
	if err != nil {
		return normalizedCreateHumanProposalRevisionInput{}, err
	}
	if input.EditedAt.IsZero() {
		input.EditedAt = time.Now().UTC()
	} else {
		input.EditedAt = input.EditedAt.UTC()
	}
	if input.EditedAt.UnixMicro() < 0 {
		return normalizedCreateHumanProposalRevisionInput{}, errors.New("proposal revision time is invalid")
	}
	input.Body = body
	input.InlineCommentsJSON = inline
	return normalizedCreateHumanProposalRevisionInput{CreateHumanProposalRevisionInput: input, ContentSHA256: contentSHA256}, nil
}

func normalizeProposalContent(body string, inlineComments []byte) (string, string, error) {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	if len(body) > maxProposalBodyBytes {
		return "", "", errors.New("proposal body exceeds maximum size")
	}
	encoded, err := json.Marshal(struct {
		Body           string          `json:"body"`
		InlineComments json.RawMessage `json:"inline_comments"`
	}{Body: body, InlineComments: inlineComments})
	if err != nil {
		return "", "", fmt.Errorf("encode proposal content: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return body, hex.EncodeToString(digest[:]), nil
}

// ProposalDecision is a human or policy disposition for one exact proposal
// revision. It is evidence, not a GitHub publication instruction.
type ProposalDecision string

const (
	ProposalDecisionApprove ProposalDecision = "approve"
	ProposalDecisionReject  ProposalDecision = "reject"
)

// ProposalDecisionActor identifies who made an immutable decision.
type ProposalDecisionActor string

const (
	ProposalDecisionActorHuman  ProposalDecisionActor = "human"
	ProposalDecisionActorPolicy ProposalDecisionActor = "policy"
)

// RecordProposalDecisionInput records one approval or rejection pinned to one
// immutable proposal revision. Reusing IdempotencyKey with identical facts is
// safe; changed facts fail closed.
type RecordProposalDecisionInput struct {
	ProposalRevisionID string
	Decision           ProposalDecision
	ActorKind          ProposalDecisionActor
	ActorID            string
	IdempotencyKey     string
	Reason             string
	DecidedAt          time.Time
}

// RecordProposalDecisionResult identifies one durable decision.
type RecordProposalDecisionResult struct {
	DecisionID string
	Created    bool
}

// RecordProposalDecision stores an evidence-bound decision. It performs no
// publication, job, event, or outbox action.
func (s *Store) RecordProposalDecision(ctx context.Context, input RecordProposalDecisionInput) (RecordProposalDecisionResult, error) {
	return s.recordProposalDecision(ctx, "", input)
}

// RecordProposalDecisionForProposal stores a decision only when the exact
// proposal revision belongs to proposalID. HTTP routes use this narrow method
// so a path cannot authorize a decision for a different proposal.
func (s *Store) RecordProposalDecisionForProposal(ctx context.Context, proposalID string, input RecordProposalDecisionInput) (RecordProposalDecisionResult, error) {
	proposalID = strings.TrimSpace(proposalID)
	if proposalID == "" || len(proposalID) > 512 {
		return RecordProposalDecisionResult{}, ErrProposalNotFound
	}
	return s.recordProposalDecision(ctx, proposalID, input)
}

func (s *Store) recordProposalDecision(ctx context.Context, expectedProposalID string, input RecordProposalDecisionInput) (RecordProposalDecisionResult, error) {
	normalized, err := normalizeRecordProposalDecisionInput(input)
	if err != nil {
		return RecordProposalDecisionResult{}, err
	}

	var result RecordProposalDecisionResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		revision, err := loadStoredProposalRevision(ctx, conn, normalized.ProposalRevisionID)
		if err != nil {
			return err
		}
		if expectedProposalID != "" && revision.ProposalID != expectedProposalID {
			return ErrProposalNotFound
		}
		target, err := loadCurrentCanonicalReviewTarget(ctx, conn, revision.ConnectionID, revision.PullRequestID)
		if err != nil {
			return err
		}
		if !revision.matchesCurrentTarget(target) {
			return errors.New("proposal revision no longer matches current canonical evidence")
		}
		existing, found, err := loadStoredDecisionByIdempotencyKey(ctx, conn, normalized.IdempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if !existing.matches(revision, normalized) {
				return fmt.Errorf("%w: key=%q", ErrProposalDecisionConflict, normalized.IdempotencyKey)
			}
			result = RecordProposalDecisionResult{DecisionID: existing.ID}
			return nil
		}
		if existing, found, err = loadStoredDecisionByProposalRevision(ctx, conn, revision.ID); err != nil {
			return err
		} else if found {
			return fmt.Errorf("%w: proposal revision=%q", ErrProposalDecisionConflict, revision.ID)
		}

		id := stableID("proposal-decision", normalized.IdempotencyKey)
		if _, err := conn.ExecContext(ctx, `
INSERT INTO decisions(
 id, proposal_id, proposal_revision_id, policy_evaluation_id, assessment_id,
 run_id, intent_id, connection_id, repository_id, pull_request_id, revision_id,
 observation_id, decision, actor_kind, actor_id, idempotency_key, reason, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?)`,
			id, revision.ProposalID, revision.ID, revision.PolicyEvaluationID, revision.AssessmentID,
			revision.RunID, revision.IntentID, revision.ConnectionID, revision.RepositoryID,
			revision.PullRequestID, revision.RevisionID, revision.ObservationID, normalized.Decision,
			normalized.ActorKind, normalized.ActorID, normalized.IdempotencyKey, normalized.Reason,
			normalized.DecidedAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert proposal decision: %w", err)
		}
		result = RecordProposalDecisionResult{DecisionID: id, Created: true}
		return nil
	})
	if err != nil {
		return RecordProposalDecisionResult{}, fmt.Errorf("record proposal decision: %w", err)
	}
	return result, nil
}

type normalizedRecordProposalDecisionInput struct{ RecordProposalDecisionInput }

func normalizeRecordProposalDecisionInput(input RecordProposalDecisionInput) (normalizedRecordProposalDecisionInput, error) {
	input.ProposalRevisionID = strings.TrimSpace(input.ProposalRevisionID)
	input.ActorID = strings.TrimSpace(input.ActorID)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.Reason = strings.ReplaceAll(strings.ReplaceAll(input.Reason, "\r\n", "\n"), "\r", "\n")
	if input.ProposalRevisionID == "" || len(input.ProposalRevisionID) > 512 ||
		(input.Decision != ProposalDecisionApprove && input.Decision != ProposalDecisionReject) ||
		(input.ActorKind != ProposalDecisionActorHuman && input.ActorKind != ProposalDecisionActorPolicy) ||
		input.ActorID == "" || len(input.ActorID) > 512 || input.IdempotencyKey == "" || len(input.IdempotencyKey) > 512 ||
		len(input.Reason) > maxProposalDecisionReasonBytes {
		return normalizedRecordProposalDecisionInput{}, errors.New("proposal decision input is invalid")
	}
	if input.DecidedAt.IsZero() {
		input.DecidedAt = time.Now().UTC()
	} else {
		input.DecidedAt = input.DecidedAt.UTC()
	}
	if input.DecidedAt.UnixMicro() < 0 {
		return normalizedRecordProposalDecisionInput{}, errors.New("proposal decision time is invalid")
	}
	return normalizedRecordProposalDecisionInput{RecordProposalDecisionInput: input}, nil
}

type storedProposal struct {
	ProposalID         string
	PolicyEvaluationID string
	AssessmentID       string
	RunID              string
	IntentID           string
	ConnectionID       string
	RepositoryID       string
	PullRequestID      string
	RevisionID         string
	ObservationID      string
}

func loadStoredProposal(ctx context.Context, conn *sql.Conn, proposalID string) (storedProposal, error) {
	var value storedProposal
	err := conn.QueryRowContext(ctx, `
SELECT proposal.id, proposal.policy_evaluation_id, proposal.assessment_id,
       proposal.run_id, proposal.intent_id, evaluation.connection_id,
       evaluation.repository_id, proposal.pull_request_id, proposal.revision_id,
       proposal.observation_id
FROM proposals AS proposal
JOIN policy_evaluations AS evaluation ON evaluation.id = proposal.policy_evaluation_id
WHERE proposal.id = ?`, proposalID).Scan(
		&value.ProposalID, &value.PolicyEvaluationID, &value.AssessmentID, &value.RunID,
		&value.IntentID, &value.ConnectionID, &value.RepositoryID, &value.PullRequestID,
		&value.RevisionID, &value.ObservationID)
	if errors.Is(err, sql.ErrNoRows) {
		return storedProposal{}, ErrProposalNotFound
	}
	if err != nil {
		return storedProposal{}, fmt.Errorf("load proposal: %w", err)
	}
	return value, nil
}

func (value storedProposal) matchesCurrentTarget(target CanonicalReviewTarget) bool {
	return value.ConnectionID == target.ConnectionID && value.RepositoryID == target.RepositoryID &&
		value.PullRequestID == target.PullRequestID && value.RevisionID == target.RevisionID &&
		value.ObservationID == target.ObservationID
}

func nextProposalRevisionNumber(ctx context.Context, conn *sql.Conn, proposalID string) (int, error) {
	var maximum int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision_number), 0) FROM proposal_revisions WHERE proposal_id = ?`, proposalID).Scan(&maximum); err != nil {
		return 0, fmt.Errorf("load latest proposal revision: %w", err)
	}
	if maximum < 1 {
		return 0, errors.New("proposal has no source revision")
	}
	return maximum + 1, nil
}

type storedProposalRevision struct {
	ID string
	storedProposal
}

func loadStoredProposalRevision(ctx context.Context, conn *sql.Conn, revisionID string) (storedProposalRevision, error) {
	var value storedProposalRevision
	err := conn.QueryRowContext(ctx, `
SELECT revision.id, proposal.id, proposal.policy_evaluation_id, proposal.assessment_id,
       proposal.run_id, proposal.intent_id, evaluation.connection_id,
       evaluation.repository_id, proposal.pull_request_id, proposal.revision_id,
       proposal.observation_id
FROM proposal_revisions AS revision
JOIN proposals AS proposal ON proposal.id = revision.proposal_id
JOIN policy_evaluations AS evaluation ON evaluation.id = proposal.policy_evaluation_id
WHERE revision.id = ?`, revisionID).Scan(
		&value.ID, &value.ProposalID, &value.PolicyEvaluationID, &value.AssessmentID,
		&value.RunID, &value.IntentID, &value.ConnectionID, &value.RepositoryID,
		&value.PullRequestID, &value.RevisionID, &value.ObservationID)
	if errors.Is(err, sql.ErrNoRows) {
		return storedProposalRevision{}, ErrProposalNotFound
	}
	if err != nil {
		return storedProposalRevision{}, fmt.Errorf("load proposal revision: %w", err)
	}
	return value, nil
}

func (value storedProposalRevision) matchesCurrentTarget(target CanonicalReviewTarget) bool {
	return value.storedProposal.matchesCurrentTarget(target)
}

type storedProposalDecision struct {
	ID                 string
	ProposalRevisionID string
	Decision           ProposalDecision
	ActorKind          ProposalDecisionActor
	ActorID            string
	IdempotencyKey     string
	Reason             sql.NullString
}

func loadStoredDecisionByIdempotencyKey(ctx context.Context, conn *sql.Conn, idempotencyKey string) (storedProposalDecision, bool, error) {
	return loadStoredProposalDecision(ctx, conn, `WHERE decision.idempotency_key = ?`, idempotencyKey)
}

func loadStoredDecisionByProposalRevision(ctx context.Context, conn *sql.Conn, proposalRevisionID string) (storedProposalDecision, bool, error) {
	return loadStoredProposalDecision(ctx, conn, `WHERE decision.proposal_revision_id = ?`, proposalRevisionID)
}

func loadStoredProposalDecision(ctx context.Context, conn *sql.Conn, predicate, value string) (storedProposalDecision, bool, error) {
	var decision storedProposalDecision
	err := conn.QueryRowContext(ctx, `
SELECT decision.id, decision.proposal_revision_id, decision.decision,
       decision.actor_kind, decision.actor_id, decision.idempotency_key, decision.reason
FROM decisions AS decision `+predicate, value).Scan(
		&decision.ID, &decision.ProposalRevisionID, &decision.Decision, &decision.ActorKind,
		&decision.ActorID, &decision.IdempotencyKey, &decision.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return storedProposalDecision{}, false, nil
	}
	if err != nil {
		return storedProposalDecision{}, false, fmt.Errorf("load proposal decision: %w", err)
	}
	return decision, true, nil
}

func (value storedProposalDecision) matches(revision storedProposalRevision, input normalizedRecordProposalDecisionInput) bool {
	return value.ProposalRevisionID == revision.ID && value.Decision == input.Decision &&
		value.ActorKind == input.ActorKind && value.ActorID == input.ActorID &&
		value.IdempotencyKey == input.IdempotencyKey && value.Reason.String == input.Reason
}
