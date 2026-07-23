package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/canonical"
)

const (
	maxPolicyInputBytes      = 64 * 1024
	maxPolicyOverridesBytes  = 16 * 1024
	maxProposalBodyBytes     = 64 * 1024
	maxProposalCommentsBytes = 64 * 1024
)

var (
	// ErrPolicyEvaluationConflict means one assessment/input snapshot is
	// already bound to different immutable policy facts.
	ErrPolicyEvaluationConflict = errors.New("policy evaluation facts conflict")
	// ErrPolicyAssessmentNotFound means no completed assessment can be safely
	// evaluated under the currently selected canonical evidence.
	ErrPolicyAssessmentNotFound = errors.New("completed assessment not found")
)

// PolicyDisposition is policy's immutable outcome classification. It does not
// itself authorize a GitHub effect.
type PolicyDisposition string

const (
	PolicyDispositionNoExternalAction    PolicyDisposition = "no_external_action"
	PolicyDispositionAutoPublishApproval PolicyDisposition = "auto_publish_approval"
	PolicyDispositionProposeApproval     PolicyDisposition = "propose_approval"
	PolicyDispositionProposeComment      PolicyDisposition = "propose_comment"
	PolicyDispositionProposeChanges      PolicyDisposition = "propose_changes"
	PolicyDispositionRequireHumanReview  PolicyDisposition = "require_human_review"
)

// RecordPolicyEvaluationInput is a resolved policy outcome for one completed
// assessment. All JSON values are normalized before their immutable digests
// are calculated.
type RecordPolicyEvaluationInput struct {
	AssessmentID         string
	PolicySetID          string
	MatchedRuleID        string
	MatchedRuleVersionID string
	ProfileID            string
	ProfileVersionID     string
	Disposition          PolicyDisposition
	InputSnapshotJSON    []byte
	SafetyOverridesJSON  []byte
	RenderedBody         string
	InlineCommentsJSON   []byte
	CreatedAt            time.Time
}

// RecordPolicyEvaluationResult identifies immutable policy evidence and, for
// proposal dispositions, its initial proposal revision.
type RecordPolicyEvaluationResult struct {
	PolicyEvaluationID string
	ProposalID         string
	ProposalRevisionID string
	Created            bool
}

// RecordPolicyEvaluation atomically stores a completed assessment's resolved
// policy evidence. Proposal dispositions, including an automatically
// publishable approval, create exactly one policy-owned initial proposal
// revision. Automatic approval also records a policy-owned approval decision.
// It creates no effects, jobs, events, outbox records, or GitHub work.
func (s *Store) RecordPolicyEvaluation(ctx context.Context, input RecordPolicyEvaluationInput) (RecordPolicyEvaluationResult, error) {
	normalized, err := normalizeRecordPolicyEvaluationInput(input)
	if err != nil {
		return RecordPolicyEvaluationResult{}, err
	}

	var result RecordPolicyEvaluationResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		assessment, err := loadCompletedPolicyAssessment(ctx, conn, normalized.AssessmentID)
		if err != nil {
			return err
		}
		target, err := loadCurrentCanonicalReviewTarget(ctx, conn, assessment.ConnectionID, assessment.PullRequestID)
		if err != nil {
			return err
		}
		if !assessment.matchesCurrentTarget(target) {
			return errors.New("assessment no longer matches current canonical evidence")
		}
		if assessment.ProfileID != normalized.ProfileID || assessment.ProfileVersionID != normalized.ProfileVersionID {
			return errors.New("policy evaluation profile differs from completed assessment profile")
		}
		if err := requireResolvedPolicyRule(ctx, conn, normalized); err != nil {
			return err
		}

		existing, found, err := loadPolicyEvaluation(ctx, conn, normalized.AssessmentID, normalized.InputSHA256)
		if err != nil {
			return err
		}
		if found {
			if !existing.matches(assessment, normalized) {
				return fmt.Errorf("%w: assessment=%q", ErrPolicyEvaluationConflict, normalized.AssessmentID)
			}
			result = existing.result()
			return nil
		}

		evaluationID := stableID("policy-evaluation", normalized.AssessmentID, normalized.InputSHA256)
		createdAt := normalized.CreatedAt.UnixMicro()
		if _, err := conn.ExecContext(ctx, `
INSERT INTO policy_evaluations(
 id, assessment_id, run_id, intent_id, connection_id, repository_id,
 pull_request_id, revision_id, observation_id, policy_set_id,
 matched_rule_id, matched_rule_version_id, profile_id, profile_version_id,
 disposition, input_json, input_sha256, safety_overrides_json,
 rendered_output_sha256, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			evaluationID, assessment.AssessmentID, assessment.RunID, assessment.IntentID,
			assessment.ConnectionID, target.RepositoryID, assessment.PullRequestID,
			assessment.RevisionID, assessment.ObservationID, normalized.PolicySetID,
			normalized.MatchedRuleID, normalized.MatchedRuleVersionID, normalized.ProfileID,
			normalized.ProfileVersionID, normalized.Disposition, normalized.InputSnapshotJSON,
			normalized.InputSHA256, normalized.SafetyOverridesJSON, normalized.RenderedOutputSHA256,
			createdAt); err != nil {
			return fmt.Errorf("insert policy evaluation: %w", err)
		}

		result = RecordPolicyEvaluationResult{PolicyEvaluationID: evaluationID, Created: true}
		if !normalized.requiresProposal() {
			return nil
		}
		alreadyProposed, err := hasProposalForEvidenceAndRule(ctx, conn, assessment, normalized)
		if err != nil {
			return err
		}
		if alreadyProposed {
			return nil
		}
		proposalID := stableID("proposal", evaluationID)
		proposalKind := normalized.proposalKind()
		if _, err := conn.ExecContext(ctx, `
INSERT INTO proposals(
 id, policy_evaluation_id, assessment_id, run_id, intent_id,
 pull_request_id, revision_id, observation_id, proposal_kind, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			proposalID, evaluationID, assessment.AssessmentID, assessment.RunID, assessment.IntentID,
			assessment.PullRequestID, assessment.RevisionID, assessment.ObservationID, proposalKind, createdAt); err != nil {
			return fmt.Errorf("insert proposal: %w", err)
		}
		revisionID := stableID("proposal-revision", proposalID, "1")
		if _, err := conn.ExecContext(ctx, `
INSERT INTO proposal_revisions(
 id, proposal_id, policy_evaluation_id, assessment_id, run_id, intent_id,
 pull_request_id, revision_id, observation_id, revision_number, editor_kind,
 body, inline_comments_json, content_sha256, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'policy', ?, ?, ?, ?)`,
			revisionID, proposalID, evaluationID, assessment.AssessmentID, assessment.RunID, assessment.IntentID,
			assessment.PullRequestID, assessment.RevisionID, assessment.ObservationID,
			normalized.RenderedBody, normalized.InlineCommentsJSON, normalized.RenderedOutputSHA256, createdAt); err != nil {
			return fmt.Errorf("insert proposal revision: %w", err)
		}
		result.ProposalID = proposalID
		result.ProposalRevisionID = revisionID
		if normalized.Disposition == PolicyDispositionAutoPublishApproval {
			decisionID := stableID("policy-auto-approval", evaluationID)
			if _, err := conn.ExecContext(ctx, `
INSERT INTO decisions(
 id, proposal_id, proposal_revision_id, policy_evaluation_id, assessment_id,
 run_id, intent_id, connection_id, repository_id, pull_request_id, revision_id,
 observation_id, decision, actor_kind, actor_id, idempotency_key, reason, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'approve', 'policy', ?, ?, ?, ?)`,
				decisionID, proposalID, revisionID, evaluationID, assessment.AssessmentID,
				assessment.RunID, assessment.IntentID, assessment.ConnectionID, target.RepositoryID,
				assessment.PullRequestID, assessment.RevisionID, assessment.ObservationID,
				"policy:"+normalized.MatchedRuleVersionID, "policy-auto-approval:"+evaluationID,
				"automatic approval authorized by immutable policy", createdAt); err != nil {
				return fmt.Errorf("insert policy auto-approval decision: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return RecordPolicyEvaluationResult{}, fmt.Errorf("record policy evaluation: %w", err)
	}
	return result, nil
}

type normalizedRecordPolicyEvaluationInput struct {
	RecordPolicyEvaluationInput
	InputSHA256          string
	RenderedOutputSHA256 string
}

func normalizeRecordPolicyEvaluationInput(input RecordPolicyEvaluationInput) (normalizedRecordPolicyEvaluationInput, error) {
	input.AssessmentID = strings.TrimSpace(input.AssessmentID)
	input.PolicySetID = strings.TrimSpace(input.PolicySetID)
	input.MatchedRuleID = strings.TrimSpace(input.MatchedRuleID)
	input.MatchedRuleVersionID = strings.TrimSpace(input.MatchedRuleVersionID)
	input.ProfileID = strings.TrimSpace(input.ProfileID)
	input.ProfileVersionID = strings.TrimSpace(input.ProfileVersionID)
	if input.AssessmentID == "" || input.PolicySetID == "" || input.MatchedRuleID == "" ||
		input.MatchedRuleVersionID == "" || input.ProfileID == "" || input.ProfileVersionID == "" ||
		!validPolicyDisposition(input.Disposition) {
		return normalizedRecordPolicyEvaluationInput{}, errors.New("policy evaluation input is invalid")
	}
	inputSnapshot, err := normalizeBoundedJSONObject(input.InputSnapshotJSON, maxPolicyInputBytes)
	if err != nil {
		return normalizedRecordPolicyEvaluationInput{}, fmt.Errorf("policy input snapshot: %w", err)
	}
	overrides, err := normalizeBoundedJSONArray(input.SafetyOverridesJSON, maxPolicyOverridesBytes)
	if err != nil {
		return normalizedRecordPolicyEvaluationInput{}, fmt.Errorf("policy safety overrides: %w", err)
	}
	inlineComments, err := normalizeBoundedJSONArray(input.InlineCommentsJSON, maxProposalCommentsBytes)
	if err != nil {
		return normalizedRecordPolicyEvaluationInput{}, fmt.Errorf("proposal inline comments: %w", err)
	}
	input.RenderedBody = strings.ReplaceAll(strings.ReplaceAll(input.RenderedBody, "\r\n", "\n"), "\r", "\n")
	if len(input.RenderedBody) > maxProposalBodyBytes {
		return normalizedRecordPolicyEvaluationInput{}, errors.New("proposal body exceeds maximum size")
	}
	input.InputSnapshotJSON = inputSnapshot
	input.SafetyOverridesJSON = overrides
	input.InlineCommentsJSON = inlineComments
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	} else {
		input.CreatedAt = input.CreatedAt.UTC()
	}
	if input.CreatedAt.UnixMicro() < 0 {
		return normalizedRecordPolicyEvaluationInput{}, errors.New("policy evaluation time is invalid")
	}
	inputDigest := sha256.Sum256(input.InputSnapshotJSON)
	rendered, err := json.Marshal(struct {
		Body           string          `json:"body"`
		InlineComments json.RawMessage `json:"inline_comments"`
	}{Body: input.RenderedBody, InlineComments: input.InlineCommentsJSON})
	if err != nil {
		return normalizedRecordPolicyEvaluationInput{}, fmt.Errorf("encode rendered proposal: %w", err)
	}
	renderedDigest := sha256.Sum256(rendered)
	return normalizedRecordPolicyEvaluationInput{
		RecordPolicyEvaluationInput: input,
		InputSHA256:                 hex.EncodeToString(inputDigest[:]), RenderedOutputSHA256: hex.EncodeToString(renderedDigest[:]),
	}, nil
}

func validPolicyDisposition(value PolicyDisposition) bool {
	switch value {
	case PolicyDispositionNoExternalAction, PolicyDispositionAutoPublishApproval,
		PolicyDispositionProposeApproval, PolicyDispositionProposeComment,
		PolicyDispositionProposeChanges, PolicyDispositionRequireHumanReview:
		return true
	default:
		return false
	}
}

func (input normalizedRecordPolicyEvaluationInput) requiresProposal() bool {
	return input.Disposition == PolicyDispositionAutoPublishApproval ||
		input.Disposition == PolicyDispositionProposeApproval ||
		input.Disposition == PolicyDispositionProposeComment ||
		input.Disposition == PolicyDispositionProposeChanges
}

func (input normalizedRecordPolicyEvaluationInput) proposalKind() string {
	switch input.Disposition {
	case PolicyDispositionAutoPublishApproval, PolicyDispositionProposeApproval:
		return "approval"
	case PolicyDispositionProposeComment:
		return "comment"
	case PolicyDispositionProposeChanges:
		return "changes"
	default:
		return ""
	}
}

// hasProposalForEvidenceAndRule prevents a repeat review of unchanged evidence
// from replacing a pending or already decided human proposal. The new policy
// evaluation remains in the immutable audit trail without another proposal.
func hasProposalForEvidenceAndRule(ctx context.Context, conn *sql.Conn, assessment completedPolicyAssessment, input normalizedRecordPolicyEvaluationInput) (bool, error) {
	var exists int
	err := conn.QueryRowContext(ctx, `
SELECT EXISTS (
 SELECT 1
 FROM proposals AS proposal
 JOIN proposal_revisions AS proposal_revision ON proposal_revision.proposal_id = proposal.id
 JOIN policy_evaluations AS evaluation ON evaluation.id = proposal.policy_evaluation_id
 WHERE evaluation.connection_id = ?
   AND proposal.pull_request_id = ?
   AND proposal.revision_id = ?
   AND proposal.observation_id = ?
   AND evaluation.matched_rule_version_id = ?
)`, assessment.ConnectionID, assessment.PullRequestID, assessment.RevisionID,
		assessment.ObservationID, input.MatchedRuleVersionID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check prior proposal for current evidence: %w", err)
	}
	return exists == 1, nil
}

func normalizeBoundedJSONObject(raw []byte, maximum int) ([]byte, error) {
	if len(raw) > maximum {
		return nil, errors.New("JSON object exceeds maximum size")
	}
	normalized, err := normalizeJSONObject(raw)
	if err != nil {
		return nil, err
	}
	if len(normalized) > maximum {
		return nil, errors.New("JSON object exceeds maximum size")
	}
	return normalized, nil
}

func normalizeBoundedJSONArray(raw []byte, maximum int) ([]byte, error) {
	if len(raw) == 0 {
		raw = []byte(`[]`)
	}
	if len(raw) > maximum {
		return nil, errors.New("JSON array exceeds maximum size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value []any
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, errors.New("must be a JSON array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil || !errors.Is(err, io.EOF) {
		return nil, errors.New("must contain one JSON array")
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode JSON array: %w", err)
	}
	if len(normalized) > maximum {
		return nil, errors.New("JSON array exceeds maximum size")
	}
	return normalized, nil
}

type completedPolicyAssessment struct {
	AssessmentID     string
	RunID            string
	IntentID         string
	ConnectionID     string
	PullRequestID    string
	RevisionID       string
	ObservationID    string
	ProfileID        string
	ProfileVersionID string
	ManifestSHA256   string
	ManifestJSON     []byte
}

func loadCompletedPolicyAssessment(ctx context.Context, conn *sql.Conn, assessmentID string) (completedPolicyAssessment, error) {
	var value completedPolicyAssessment
	err := conn.QueryRowContext(ctx, `
SELECT assessment.id, assessment.run_id, assessment.intent_id,
       intent.connection_id, assessment.pull_request_id, assessment.revision_id,
       assessment.observation_id, intent.profile_id, intent.profile_version_id,
       context.manifest_sha256, context.manifest_json
FROM assessments AS assessment
JOIN review_runs AS run ON run.id = assessment.run_id AND run.intent_id = assessment.intent_id
JOIN review_intents AS intent ON intent.id = run.intent_id
JOIN review_run_contexts AS context ON context.run_id = run.id
WHERE assessment.id = ?
  AND EXISTS (
      SELECT 1 FROM review_run_events AS event
      WHERE event.run_id = run.id AND event.event_kind = 'succeeded'
  )`, assessmentID).Scan(
		&value.AssessmentID, &value.RunID, &value.IntentID, &value.ConnectionID,
		&value.PullRequestID, &value.RevisionID, &value.ObservationID,
		&value.ProfileID, &value.ProfileVersionID, &value.ManifestSHA256, &value.ManifestJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return completedPolicyAssessment{}, ErrPolicyAssessmentNotFound
	}
	if err != nil {
		return completedPolicyAssessment{}, fmt.Errorf("load completed assessment: %w", err)
	}
	if _, err := canonicalManifestSHA256(value.ManifestJSON, value.ManifestSHA256); err != nil {
		return completedPolicyAssessment{}, errors.New("completed assessment manifest context is invalid")
	}
	return value, nil
}

func (value completedPolicyAssessment) matchesCurrentTarget(target CanonicalReviewTarget) bool {
	return value.ConnectionID == target.ConnectionID && value.PullRequestID == target.PullRequestID &&
		value.RevisionID == target.RevisionID && value.ObservationID == target.ObservationID &&
		value.ManifestSHA256 == target.ManifestSHA256 && bytes.Equal(value.ManifestJSON, target.ManifestJSON)
}

func canonicalManifestSHA256(manifest []byte, expected string) (string, error) {
	verified, err := canonical.Validate(manifest)
	if err != nil || verified.ManifestSHA256 != expected {
		return "", errors.New("invalid canonical manifest")
	}
	return verified.ManifestSHA256, nil
}

func requireResolvedPolicyRule(ctx context.Context, conn *sql.Conn, input normalizedRecordPolicyEvaluationInput) error {
	var policySetID, ruleID, profileID, profileVersionID string
	var enabled int
	err := conn.QueryRowContext(ctx, `
SELECT version.policy_set_id, version.rule_id, version.profile_id, version.profile_version_id, rule.enabled
FROM watch_rule_versions AS version
JOIN watch_rules AS rule ON rule.id = version.rule_id
JOIN policy_sets AS policy_set ON policy_set.id = version.policy_set_id
WHERE version.id = ? AND rule.current_version_id = version.id`, input.MatchedRuleVersionID).Scan(
		&policySetID, &ruleID, &profileID, &profileVersionID, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("resolved policy rule version does not exist or is not current")
	}
	if err != nil {
		return fmt.Errorf("load resolved policy rule: %w", err)
	}
	if enabled != 1 || policySetID != input.PolicySetID || ruleID != input.MatchedRuleID {
		return errors.New("resolved policy rule differs from policy evaluation")
	}
	if profileID != input.ProfileID || profileVersionID != input.ProfileVersionID {
		return errors.New("resolved policy rule profile differs from policy evaluation")
	}
	return requireReviewProfileVersion(ctx, conn, input.ProfileID, input.ProfileVersionID)
}

type storedPolicyEvaluation struct {
	ID                    string
	AssessmentID          string
	RunID                 string
	IntentID              string
	ConnectionID          string
	RepositoryID          string
	PullRequestID         string
	RevisionID            string
	ObservationID         string
	PolicySetID           string
	MatchedRuleID         string
	MatchedRuleVersionID  string
	ProfileID             string
	ProfileVersionID      string
	Disposition           PolicyDisposition
	InputJSON             []byte
	InputSHA256           string
	SafetyOverridesJSON   []byte
	RenderedOutputSHA256  string
	ProposalID            sql.NullString
	ProposalRevisionID    sql.NullString
	ProposalKind          sql.NullString
	ProposalBody          sql.NullString
	InlineCommentsJSON    []byte
	ProposalContentSHA256 sql.NullString
}

func loadPolicyEvaluation(ctx context.Context, conn *sql.Conn, assessmentID, inputSHA256 string) (storedPolicyEvaluation, bool, error) {
	var value storedPolicyEvaluation
	err := conn.QueryRowContext(ctx, `
SELECT evaluation.id, evaluation.assessment_id, evaluation.run_id, evaluation.intent_id,
       evaluation.connection_id, evaluation.repository_id, evaluation.pull_request_id,
       evaluation.revision_id, evaluation.observation_id, evaluation.policy_set_id,
       evaluation.matched_rule_id, evaluation.matched_rule_version_id,
       evaluation.profile_id, evaluation.profile_version_id, evaluation.disposition,
       evaluation.input_json, evaluation.input_sha256, evaluation.safety_overrides_json,
       evaluation.rendered_output_sha256,
       proposal.id, proposal_revision.id, proposal.proposal_kind, proposal_revision.body,
       proposal_revision.inline_comments_json, proposal_revision.content_sha256
FROM policy_evaluations AS evaluation
LEFT JOIN proposals AS proposal ON proposal.policy_evaluation_id = evaluation.id
LEFT JOIN proposal_revisions AS proposal_revision
  ON proposal_revision.proposal_id = proposal.id AND proposal_revision.revision_number = 1
WHERE evaluation.assessment_id = ? AND evaluation.input_sha256 = ?`, assessmentID, inputSHA256).Scan(
		&value.ID, &value.AssessmentID, &value.RunID, &value.IntentID,
		&value.ConnectionID, &value.RepositoryID, &value.PullRequestID,
		&value.RevisionID, &value.ObservationID, &value.PolicySetID,
		&value.MatchedRuleID, &value.MatchedRuleVersionID,
		&value.ProfileID, &value.ProfileVersionID, &value.Disposition,
		&value.InputJSON, &value.InputSHA256, &value.SafetyOverridesJSON, &value.RenderedOutputSHA256,
		&value.ProposalID, &value.ProposalRevisionID, &value.ProposalKind, &value.ProposalBody,
		&value.InlineCommentsJSON, &value.ProposalContentSHA256,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedPolicyEvaluation{}, false, nil
	}
	if err != nil {
		return storedPolicyEvaluation{}, false, fmt.Errorf("load policy evaluation: %w", err)
	}
	return value, true, nil
}

func (value storedPolicyEvaluation) matches(assessment completedPolicyAssessment, input normalizedRecordPolicyEvaluationInput) bool {
	if value.AssessmentID != assessment.AssessmentID || value.RunID != assessment.RunID ||
		value.IntentID != assessment.IntentID || value.ConnectionID != assessment.ConnectionID ||
		value.RepositoryID == "" || value.PullRequestID != assessment.PullRequestID ||
		value.RevisionID != assessment.RevisionID || value.ObservationID != assessment.ObservationID ||
		value.PolicySetID != input.PolicySetID || value.MatchedRuleID != input.MatchedRuleID ||
		value.MatchedRuleVersionID != input.MatchedRuleVersionID || value.ProfileID != input.ProfileID ||
		value.ProfileVersionID != input.ProfileVersionID || value.Disposition != input.Disposition ||
		!bytes.Equal(value.InputJSON, input.InputSnapshotJSON) || value.InputSHA256 != input.InputSHA256 ||
		!bytes.Equal(value.SafetyOverridesJSON, input.SafetyOverridesJSON) ||
		value.RenderedOutputSHA256 != input.RenderedOutputSHA256 {
		return false
	}
	if !input.requiresProposal() {
		return !value.ProposalID.Valid && !value.ProposalRevisionID.Valid
	}
	return value.ProposalID.Valid && value.ProposalRevisionID.Valid &&
		value.ProposalKind.String == input.proposalKind() && value.ProposalBody.String == input.RenderedBody &&
		bytes.Equal(value.InlineCommentsJSON, input.InlineCommentsJSON) &&
		value.ProposalContentSHA256.String == input.RenderedOutputSHA256
}

func (value storedPolicyEvaluation) result() RecordPolicyEvaluationResult {
	return RecordPolicyEvaluationResult{
		PolicyEvaluationID: value.ID, ProposalID: value.ProposalID.String,
		ProposalRevisionID: value.ProposalRevisionID.String,
	}
}
