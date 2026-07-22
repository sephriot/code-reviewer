// Package policyevaluate turns completed, current review evidence into an
// immutable policy evaluation. It intentionally has no job or GitHub write
// capability.
package policyevaluate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/assessment"
	"github.com/sephriot/code-reviewer/internal/application/policy"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

var (
	// ErrEvaluationTerminal means policy cannot safely evaluate a terminal PR.
	ErrEvaluationTerminal = errors.New("policy evaluation pull request is terminal")
)

// Request identifies the immutable completed assessment and the explicitly
// selected active policy rule. Rule matching remains outside this service.
type Request struct {
	AssessmentID  string
	RuleKey       string
	RuleVersionID string
	EvaluatedAt   time.Time
}

// Reader loads only the facts required to evaluate a selected policy rule.
type Reader interface {
	LoadPolicyEvaluationTarget(context.Context, string) (sqlite.PolicyEvaluationTarget, error)
	LoadActivePolicyRule(context.Context, string, string) (sqlite.ActivePolicyRule, error)
}

// Recorder appends a fully resolved immutable policy evaluation.
type Recorder interface {
	RecordPolicyEvaluation(context.Context, sqlite.RecordPolicyEvaluationInput) (sqlite.RecordPolicyEvaluationResult, error)
}

// EventAppender commits a policy outcome notification intent and its durable
// outbox delivery. It is optional so read-only or historical evaluation flows
// retain their existing no-notification behavior.
type EventAppender interface {
	AppendEventWithOutbox(context.Context, sqlite.DomainEventInput, []sqlite.OutboxInput) (sqlite.AppendedEvent, error)
}

// Service is the bounded policy evaluation application boundary.
type Service struct {
	Reader   Reader
	Recorder Recorder
	Events   EventAppender
	Now      func() time.Time
}

// Result joins the pure policy outcome to its immutable persistence result.
type Result struct {
	Target     sqlite.PolicyEvaluationTarget
	Rule       sqlite.ActivePolicyRule
	Outcome    policy.Result
	Evaluation sqlite.RecordPolicyEvaluationResult
}

// Evaluate loads the exact completed assessment and active rule, applies the
// pure policy function, then persists its deterministic snapshot and proposal.
func (s Service) Evaluate(ctx context.Context, request Request) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("policy evaluation context is required")
	}
	if s.Reader == nil || s.Recorder == nil {
		return Result{}, errors.New("policy evaluation dependencies are required")
	}
	request.AssessmentID = strings.TrimSpace(request.AssessmentID)
	request.RuleKey = strings.TrimSpace(request.RuleKey)
	request.RuleVersionID = strings.TrimSpace(request.RuleVersionID)
	if request.AssessmentID == "" || request.RuleKey == "" || request.RuleVersionID == "" {
		return Result{}, errors.New("assessment ID, rule key, and rule version ID are required")
	}

	target, err := s.Reader.LoadPolicyEvaluationTarget(ctx, request.AssessmentID)
	if err != nil {
		return Result{}, fmt.Errorf("load policy evaluation target: %w", err)
	}
	if target.AssessmentID != request.AssessmentID {
		return Result{}, errors.New("policy evaluation target does not match requested assessment")
	}
	rule, err := s.Reader.LoadActivePolicyRule(ctx, request.RuleKey, request.RuleVersionID)
	if err != nil {
		return Result{}, fmt.Errorf("load active policy rule: %w", err)
	}
	if rule.RuleKey != request.RuleKey || rule.VersionID != request.RuleVersionID {
		return Result{}, errors.New("active policy rule does not match requested rule")
	}
	if rule.ProfileID != target.ProfileID || rule.ProfileVersionID != target.ProfileVersionID {
		return Result{}, errors.New("policy rule profile differs from completed assessment")
	}
	configured, err := parsePublicationPolicy(rule.PublicationJSON)
	if err != nil {
		return Result{}, fmt.Errorf("policy rule publication policy: %w", err)
	}

	facts := policy.Facts{
		AuthoredByMe:    target.Facts.AuthoredByMe,
		Terminal:        target.Facts.Terminal,
		Draft:           target.Facts.Draft,
		EvidenceCurrent: target.Facts.EvidenceCurrent,
		Coverage:        target.Facts.Coverage,
	}
	if facts.Terminal {
		return Result{}, ErrEvaluationTerminal
	}
	outcome, err := policy.Evaluate(policy.Input{Assessment: target.Assessment, Facts: facts, Policy: configured})
	if err != nil {
		return Result{}, fmt.Errorf("evaluate policy: %w", err)
	}

	snapshot, err := marshalSnapshot(target, rule, facts, configured)
	if err != nil {
		return Result{}, err
	}
	overrides, err := json.Marshal(outcome.SafetyOverrides)
	if err != nil {
		return Result{}, fmt.Errorf("encode policy safety overrides: %w", err)
	}
	body, comments, err := marshalProposal(outcome.Proposal)
	if err != nil {
		return Result{}, err
	}
	at := request.EvaluatedAt.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
		if s.Now != nil {
			at = s.Now().UTC()
		}
	}
	recorded, err := s.Recorder.RecordPolicyEvaluation(ctx, sqlite.RecordPolicyEvaluationInput{
		AssessmentID: target.AssessmentID, PolicySetID: rule.PolicySetID,
		MatchedRuleID: rule.RuleID, MatchedRuleVersionID: rule.VersionID,
		ProfileID: target.ProfileID, ProfileVersionID: target.ProfileVersionID,
		Disposition: sqlite.PolicyDisposition(outcome.Disposition), InputSnapshotJSON: snapshot,
		SafetyOverridesJSON: overrides, RenderedBody: body, InlineCommentsJSON: comments, CreatedAt: at,
	})
	if err != nil {
		return Result{}, fmt.Errorf("record policy evaluation: %w", err)
	}
	if s.Events != nil {
		if err := s.appendNotificationIntent(ctx, recorded, outcome.Disposition); err != nil {
			return Result{}, err
		}
	}
	return Result{Target: target, Rule: rule, Outcome: outcome, Evaluation: recorded}, nil
}

func (s Service) appendNotificationIntent(ctx context.Context, recorded sqlite.RecordPolicyEvaluationResult, disposition policy.Disposition) error {
	if recorded.PolicyEvaluationID == "" {
		return errors.New("recorded policy evaluation has no identity")
	}
	eventID := notificationEventID(recorded.PolicyEvaluationID)
	notificationPayload, err := json.Marshal(struct {
		PolicyEvaluationID string `json:"policy_evaluation_id"`
		ProposalID         string `json:"proposal_id,omitempty"`
		Disposition        string `json:"disposition"`
	}{recorded.PolicyEvaluationID, recorded.ProposalID, string(disposition)})
	if err != nil {
		return fmt.Errorf("encode policy notification payload: %w", err)
	}
	outboxPayload, err := json.Marshal(struct {
		DomainEventID string          `json:"domain_event_id"`
		DedupeKey     string          `json:"dedupe_key"`
		Payload       json.RawMessage `json:"payload"`
	}{eventID, "policy-evaluation:" + recorded.PolicyEvaluationID, notificationPayload})
	if err != nil {
		return fmt.Errorf("encode policy notification outbox payload: %w", err)
	}
	if _, err := s.Events.AppendEventWithOutbox(ctx, sqlite.DomainEventInput{
		ID: eventID, AggregateType: "policy_evaluation", AggregateID: recorded.PolicyEvaluationID,
		EventType: "policy.evaluated", EventVersion: 1, Payload: notificationPayload,
	}, []sqlite.OutboxInput{{Topic: "notification.dispatch.v1", Payload: outboxPayload}}); err != nil {
		return fmt.Errorf("append policy notification intent: %w", err)
	}
	return nil
}

func notificationEventID(policyEvaluationID string) string {
	digest := sha256.Sum256([]byte("policy-evaluated-notification:v1:" + policyEvaluationID))
	return "notification-event-" + hex.EncodeToString(digest[:])
}

type publicationConfig struct {
	AllowAutomaticApproval bool                                      `json:"allow_automatic_approval"`
	Matrix                 map[assessment.Verdict]policy.Disposition `json:"matrix"`
}

func parsePublicationPolicy(raw []byte) (policy.PublicationPolicy, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config publicationConfig
	if err := decoder.Decode(&config); err != nil {
		return policy.PublicationPolicy{}, errors.New("must be one known JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return policy.PublicationPolicy{}, errors.New("must be one known JSON object")
	}
	if config.Matrix == nil {
		config.Matrix = map[assessment.Verdict]policy.Disposition{}
	}
	for verdict, disposition := range config.Matrix {
		if !knownVerdict(verdict) || !knownDisposition(disposition) {
			return policy.PublicationPolicy{}, errors.New("matrix contains unsupported verdict or disposition")
		}
		if disposition == policy.DispositionAutoPublishApproval && verdict != assessment.VerdictPass {
			return policy.PublicationPolicy{}, errors.New("automatic approval requires pass verdict")
		}
	}
	return policy.PublicationPolicy{AllowAutomaticApproval: config.AllowAutomaticApproval, Matrix: config.Matrix}, nil
}

func knownVerdict(value assessment.Verdict) bool {
	return value == assessment.VerdictPass || value == assessment.VerdictConcerns || value == assessment.VerdictChangesRequired || value == assessment.VerdictInconclusive
}

func knownDisposition(value policy.Disposition) bool {
	switch value {
	case policy.DispositionNoExternalAction, policy.DispositionAutoPublishApproval, policy.DispositionProposeApproval, policy.DispositionProposeComment, policy.DispositionProposeChanges, policy.DispositionRequireHumanReview:
		return true
	default:
		return false
	}
}

func marshalSnapshot(target sqlite.PolicyEvaluationTarget, rule sqlite.ActivePolicyRule, facts policy.Facts, configured policy.PublicationPolicy) ([]byte, error) {
	type snapshotFacts struct {
		AuthoredByMe    bool                `json:"authored_by_me"`
		Terminal        bool                `json:"terminal"`
		Draft           bool                `json:"draft"`
		EvidenceCurrent bool                `json:"evidence_current"`
		Coverage        assessment.Coverage `json:"coverage"`
	}
	type snapshotPublication struct {
		AllowAutomaticApproval bool                                      `json:"allow_automatic_approval"`
		Matrix                 map[assessment.Verdict]policy.Disposition `json:"matrix"`
	}
	value := struct {
		FormatVersion    int                 `json:"format_version"`
		AssessmentID     string              `json:"assessment_id"`
		AssessmentSHA256 string              `json:"assessment_sha256"`
		Facts            snapshotFacts       `json:"facts"`
		PolicySetID      string              `json:"policy_set_id"`
		RuleID           string              `json:"rule_id"`
		RuleVersionID    string              `json:"rule_version_id"`
		RuleKey          string              `json:"rule_key"`
		Publication      snapshotPublication `json:"publication"`
	}{
		1, target.AssessmentID, target.OutputSHA256,
		snapshotFacts{facts.AuthoredByMe, facts.Terminal, facts.Draft, facts.EvidenceCurrent, facts.Coverage},
		rule.PolicySetID, rule.RuleID, rule.VersionID, rule.RuleKey,
		snapshotPublication{configured.AllowAutomaticApproval, configured.Matrix},
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode policy input snapshot: %w", err)
	}
	return encoded, nil
}

type inlineComment struct {
	Path      string          `json:"path"`
	StartLine int             `json:"start_line"`
	EndLine   int             `json:"end_line"`
	Side      assessment.Side `json:"side"`
	SHA       string          `json:"sha"`
	Body      string          `json:"body"`
}

func marshalProposal(proposal *policy.Proposal) (string, []byte, error) {
	if proposal == nil {
		return "", []byte(`[]`), nil
	}
	comments := make([]inlineComment, 0, len(proposal.InlineComments))
	for _, comment := range proposal.InlineComments {
		comments = append(comments, inlineComment{Path: comment.Path, StartLine: comment.StartLine, EndLine: comment.EndLine, Side: comment.Side, SHA: comment.SHA, Body: comment.Body})
	}
	encoded, err := json.Marshal(comments)
	if err != nil {
		return "", nil, fmt.Errorf("encode policy proposal comments: %w", err)
	}
	return proposal.Body, encoded, nil
}
