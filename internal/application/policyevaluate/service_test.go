package policyevaluate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/assessment"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

func TestEvaluateRecordsDeterministicPolicyOutcome(t *testing.T) {
	target := testTarget()
	rule := testRule()
	reader := readerFunc{target: target, rule: rule}
	var recorded sqlite.RecordPolicyEvaluationInput
	service := Service{
		Reader: reader,
		Recorder: recorderFunc(func(_ context.Context, input sqlite.RecordPolicyEvaluationInput) (sqlite.RecordPolicyEvaluationResult, error) {
			recorded = input
			return sqlite.RecordPolicyEvaluationResult{PolicyEvaluationID: "evaluation-1", ProposalID: "proposal-1", Created: true}, nil
		}),
		Now: func() time.Time { return time.Unix(100, 0).UTC() },
	}

	result, err := service.Evaluate(context.Background(), Request{AssessmentID: target.AssessmentID, RuleKey: rule.RuleKey, RuleVersionID: rule.VersionID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Evaluation.PolicyEvaluationID != "evaluation-1" || result.Outcome.Disposition != "propose_changes" {
		t.Fatalf("result = %+v", result)
	}
	if recorded.AssessmentID != target.AssessmentID || recorded.PolicySetID != rule.PolicySetID ||
		recorded.MatchedRuleID != rule.RuleID || recorded.MatchedRuleVersionID != rule.VersionID ||
		recorded.ProfileID != target.ProfileID || recorded.ProfileVersionID != target.ProfileVersionID ||
		recorded.Disposition != sqlite.PolicyDispositionProposeChanges || recorded.RenderedBody == "" ||
		string(recorded.SafetyOverridesJSON) != "[]" || recorded.CreatedAt != time.Unix(100, 0).UTC() {
		t.Fatalf("recorded = %+v", recorded)
	}
	var snapshot map[string]any
	if err := json.Unmarshal(recorded.InputSnapshotJSON, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["assessment_id"] != target.AssessmentID || snapshot["rule_key"] != rule.RuleKey || snapshot["format_version"] != float64(1) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if string(recorded.InlineCommentsJSON) != `[{"path":"internal/example.go","start_line":7,"end_line":7,"side":"RIGHT","sha":"1111111111111111111111111111111111111111","body":"[high/correctness] Add nil guard"}]` {
		t.Fatalf("inline comments = %s", recorded.InlineCommentsJSON)
	}
}

func TestEvaluateAppendsIdempotentNotificationIntentWhenConfigured(t *testing.T) {
	target := testTarget()
	rule := testRule()
	events := &eventAppender{}
	service := Service{
		Reader: readerFunc{target: target, rule: rule},
		Recorder: recorderFunc(func(context.Context, sqlite.RecordPolicyEvaluationInput) (sqlite.RecordPolicyEvaluationResult, error) {
			return sqlite.RecordPolicyEvaluationResult{PolicyEvaluationID: "evaluation-1", ProposalID: "proposal-1"}, nil
		}),
		Events: events,
	}
	if _, err := service.Evaluate(context.Background(), Request{AssessmentID: target.AssessmentID, RuleKey: rule.RuleKey, RuleVersionID: rule.VersionID}); err != nil {
		t.Fatal(err)
	}
	if events.event.ID != notificationEventID("evaluation-1") || events.event.AggregateType != "policy_evaluation" || events.event.AggregateID != "evaluation-1" || events.event.EventType != "policy.evaluated" || len(events.outbox) != 1 || events.outbox[0].Topic != "notification.dispatch.v1" {
		t.Fatalf("event=%+v outbox=%+v", events.event, events.outbox)
	}
	var outbox struct {
		DomainEventID string          `json:"domain_event_id"`
		DedupeKey     string          `json:"dedupe_key"`
		Payload       json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(events.outbox[0].Payload, &outbox); err != nil {
		t.Fatal(err)
	}
	if outbox.DomainEventID != events.event.ID || outbox.DedupeKey != "policy-evaluation:evaluation-1" || string(outbox.Payload) != `{"policy_evaluation_id":"evaluation-1","proposal_id":"proposal-1","disposition":"propose_changes"}` {
		t.Fatalf("outbox=%+v", outbox)
	}
}

func TestEvaluateReturnsNotificationWriteFailureAfterRecording(t *testing.T) {
	target := testTarget()
	service := Service{
		Reader: readerFunc{target: target, rule: testRule()},
		Recorder: recorderFunc(func(context.Context, sqlite.RecordPolicyEvaluationInput) (sqlite.RecordPolicyEvaluationResult, error) {
			return sqlite.RecordPolicyEvaluationResult{PolicyEvaluationID: "evaluation-1"}, nil
		}),
		Events: &eventAppender{err: errors.New("database offline")},
	}
	if _, err := service.Evaluate(context.Background(), Request{AssessmentID: target.AssessmentID, RuleKey: "assigned-default", RuleVersionID: "rule-version-1"}); err == nil || !strings.Contains(err.Error(), "append policy notification") {
		t.Fatalf("error=%v", err)
	}
}

func TestEvaluateRejectsUnsafePublicationConfigBeforeRecording(t *testing.T) {
	target := testTarget()
	rule := testRule()
	rule.PublicationJSON = []byte(`{"matrix":{"pass":"auto_publish_approval"},"unexpected":true}`)
	recorder := recorderFunc(func(context.Context, sqlite.RecordPolicyEvaluationInput) (sqlite.RecordPolicyEvaluationResult, error) {
		t.Fatal("recorder called")
		return sqlite.RecordPolicyEvaluationResult{}, nil
	})
	service := Service{Reader: readerFunc{target: target, rule: rule}, Recorder: recorder}

	_, err := service.Evaluate(context.Background(), Request{AssessmentID: target.AssessmentID, RuleKey: rule.RuleKey, RuleVersionID: rule.VersionID})
	if err == nil || !strings.Contains(err.Error(), "publication policy") {
		t.Fatalf("error = %v", err)
	}
}

func TestEvaluateDoesNotRecordWhenPolicyFactsAreTerminal(t *testing.T) {
	target := testTarget()
	target.Facts.Terminal = true
	called := false
	service := Service{
		Reader: readerFunc{target: target, rule: testRule()},
		Recorder: recorderFunc(func(context.Context, sqlite.RecordPolicyEvaluationInput) (sqlite.RecordPolicyEvaluationResult, error) {
			called = true
			return sqlite.RecordPolicyEvaluationResult{}, nil
		}),
	}

	_, err := service.Evaluate(context.Background(), Request{AssessmentID: target.AssessmentID, RuleKey: "assigned-default", RuleVersionID: "rule-version-1"})
	if !errors.Is(err, ErrEvaluationTerminal) || called {
		t.Fatalf("error = %v called=%v", err, called)
	}
}

func TestEvaluateRequiresRuleProfileToMatchAssessment(t *testing.T) {
	target := testTarget()
	rule := testRule()
	rule.ProfileVersionID = "different"
	service := Service{Reader: readerFunc{target: target, rule: rule}, Recorder: recorderFunc(func(context.Context, sqlite.RecordPolicyEvaluationInput) (sqlite.RecordPolicyEvaluationResult, error) {
		t.Fatal("recorder called")
		return sqlite.RecordPolicyEvaluationResult{}, nil
	})}
	_, err := service.Evaluate(context.Background(), Request{AssessmentID: target.AssessmentID, RuleKey: rule.RuleKey, RuleVersionID: rule.VersionID})
	if err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("error = %v", err)
	}
}

type readerFunc struct {
	target sqlite.PolicyEvaluationTarget
	rule   sqlite.ActivePolicyRule
	err    error
}

func (r readerFunc) LoadPolicyEvaluationTarget(context.Context, string) (sqlite.PolicyEvaluationTarget, error) {
	if r.err != nil {
		return sqlite.PolicyEvaluationTarget{}, r.err
	}
	return r.target, nil
}

func (r readerFunc) LoadActivePolicyRule(context.Context, string, string) (sqlite.ActivePolicyRule, error) {
	if r.err != nil {
		return sqlite.ActivePolicyRule{}, r.err
	}
	return r.rule, nil
}

type recorderFunc func(context.Context, sqlite.RecordPolicyEvaluationInput) (sqlite.RecordPolicyEvaluationResult, error)

func (f recorderFunc) RecordPolicyEvaluation(ctx context.Context, input sqlite.RecordPolicyEvaluationInput) (sqlite.RecordPolicyEvaluationResult, error) {
	return f(ctx, input)
}

type eventAppender struct {
	event  sqlite.DomainEventInput
	outbox []sqlite.OutboxInput
	err    error
}

func (r *eventAppender) AppendEventWithOutbox(_ context.Context, event sqlite.DomainEventInput, outbox []sqlite.OutboxInput) (sqlite.AppendedEvent, error) {
	r.event, r.outbox = event, outbox
	return sqlite.AppendedEvent{EventID: event.ID}, r.err
}

func testTarget() sqlite.PolicyEvaluationTarget {
	coverage := assessment.Coverage{Status: assessment.CoverageComplete, ChangedFilesTotal: 1, ReviewedFiles: 1, Omitted: []assessment.Omission{}}
	return sqlite.PolicyEvaluationTarget{
		AssessmentID: "assessment-1", OutputSHA256: strings.Repeat("a", 64), ProfileID: "profile-1", ProfileVersionID: "profile-version-1",
		Assessment: assessment.Result{Assessment: assessment.Assessment{Version: assessment.Version1, Verdict: assessment.VerdictChangesRequired, Summary: "Nil guard is missing", Confidence: assessment.ConfidenceHigh, Limitations: []string{}, Coverage: coverage, Findings: []assessment.Finding{{ClientID: "finding-1", Severity: assessment.SeverityHigh, Category: assessment.CategoryCorrectness, Message: "Add nil guard", Anchor: &assessment.Anchor{Path: "internal/example.go", StartLine: 7, EndLine: 7, Side: assessment.SideRight, SHA: strings.Repeat("1", 40)}}}}, ValidationWarnings: []assessment.ValidationWarning{}},
		Facts:      sqlite.PolicyEvaluationFacts{Coverage: coverage, EvidenceCurrent: true},
	}
}

func testRule() sqlite.ActivePolicyRule {
	return sqlite.ActivePolicyRule{PolicySetID: "policy-set-1", RuleID: "rule-1", VersionID: "rule-version-1", RuleKey: "assigned-default", ProfileID: "profile-1", ProfileVersionID: "profile-version-1", PublicationJSON: []byte(`{"allow_automatic_approval":false,"matrix":{"changes_required":"propose_changes"}}`)}
}
