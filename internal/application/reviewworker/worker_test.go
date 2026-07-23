package reviewworker

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/adapters/engine"
	githubadapter "github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/application/assessment"
	"github.com/sephriot/code-reviewer/internal/application/reviewexecute"
	"github.com/sephriot/code-reviewer/internal/application/watchrule"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

func TestHandlerExecutesPreparedRun(t *testing.T) {
	var executed string
	events := &eventRecorder{}
	handler := Handler{
		Executor: executorFunc(func(_ context.Context, runID string) (reviewexecute.Result, error) {
			executed = runID
			return reviewexecute.Result{}, nil
		}),
		Events: events,
	}
	if err := handler.Handle(context.Background(), reviewJob(`{"run_id":"run-1"}`)); err != nil {
		t.Fatal(err)
	}
	if executed != "run-1" || len(events.inputs) != 0 {
		t.Fatalf("executed=%q events=%+v", executed, events.inputs)
	}
}

func TestHandlerEvaluatesCurrentMatchingAutomaticPolicy(t *testing.T) {
	store := automaticPolicyStore{target: automaticWatchTarget("automatic", "profile-1", "profile-version-1")}
	handler := Handler{
		Executor: executorFunc(func(context.Context, string) (reviewexecute.Result, error) {
			return automaticExecutionResult("automatic"), nil
		}),
		Events:               &eventRecorder{},
		AutomaticPolicyStore: &store,
		Now:                  func() time.Time { return time.Unix(7, 0).UTC() },
	}
	if err := handler.Handle(context.Background(), reviewJob(`{"run_id":"run-1"}`)); err != nil {
		t.Fatal(err)
	}
	if len(store.recorded) != 1 {
		t.Fatalf("policy records=%+v", store.recorded)
	}
	if len(store.events) != 1 || store.events[0].EventType != "policy.evaluated" {
		t.Fatalf("policy notification events=%+v", store.events)
	}
	recorded := store.recorded[0]
	if recorded.AssessmentID != "assessment-1" || recorded.MatchedRuleID != "rule-1" ||
		recorded.MatchedRuleVersionID != "rule-version-1" || recorded.CreatedAt != time.Unix(7, 0).UTC() {
		t.Fatalf("recorded=%+v", recorded)
	}
}

func TestHandlerPublishesOnlyAutomaticApprovalOutcome(t *testing.T) {
	store := automaticPolicyStore{target: automaticWatchTarget("automatic", "profile-1", "profile-version-1"), publicationJSON: []byte(`{"allow_automatic_approval":true,"matrix":{"pass":"auto_publish_approval"}}`)}
	publisher := &automaticPublisher{}
	handler := Handler{
		Executor: executorFunc(func(context.Context, string) (reviewexecute.Result, error) {
			return automaticExecutionResult("automatic"), nil
		}),
		Events: &eventRecorder{}, AutomaticPolicyStore: &store, AutomaticPublication: publisher,
	}
	if err := handler.Handle(context.Background(), reviewJob(`{"run_id":"run-1"}`)); err != nil {
		t.Fatal(err)
	}
	if publisher.revisionID != "proposal-revision-1" {
		t.Fatalf("publisher=%+v", publisher)
	}
}

func TestHandlerSkipsAutomaticPolicyUnlessCurrentRuleAndProfileStillMatch(t *testing.T) {
	for _, test := range []struct {
		name         string
		execution    string
		trigger      string
		profileID    string
		profileVerID string
	}{
		{name: "manual execution", execution: "manual", trigger: "automatic", profileID: "profile-1", profileVerID: "profile-version-1"},
		{name: "manual selected rule", execution: "automatic", trigger: "manual", profileID: "profile-1", profileVerID: "profile-version-1"},
		{name: "profile changed", execution: "automatic", trigger: "automatic", profileID: "other", profileVerID: "profile-version-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := automaticPolicyStore{target: automaticWatchTarget(test.trigger, test.profileID, test.profileVerID)}
			handler := Handler{
				Executor: executorFunc(func(context.Context, string) (reviewexecute.Result, error) {
					return automaticExecutionResult(test.execution), nil
				}),
				Events:               &eventRecorder{},
				AutomaticPolicyStore: &store,
			}
			if err := handler.Handle(context.Background(), reviewJob(`{"run_id":"run-1"}`)); err != nil {
				t.Fatal(err)
			}
			if len(store.recorded) != 0 {
				t.Fatalf("policy records=%+v", store.recorded)
			}
		})
	}
}

func TestHandlerReturnsPolicySelectionErrorsAfterReviewSucceeds(t *testing.T) {
	store := automaticPolicyStore{target: sqlite.AutomaticWatchRuleTarget{
		ConnectionID: "connection-1", PullRequestID: "pr-1", Facts: watchrule.Facts{RepositoryID: 1},
		Rules: []sqlite.AutomaticWatchRule{{VersionID: "rule-version-1", Priority: 0, MatchJSON: []byte(`{"unknown":true}`)}},
	}}
	handler := Handler{
		Executor: executorFunc(func(context.Context, string) (reviewexecute.Result, error) {
			return automaticExecutionResult("automatic"), nil
		}),
		Events:               &eventRecorder{},
		AutomaticPolicyStore: &store,
	}
	err := handler.Handle(context.Background(), reviewJob(`{"run_id":"run-1"}`))
	if err == nil || !worker.IsPermanent(err) || !strings.Contains(err.Error(), "select automatic policy rule") || len(store.recorded) != 0 {
		t.Fatalf("err=%v records=%+v", err, store.recorded)
	}
}

func TestHandlerRejectsMalformedPayloadBeforeExecution(t *testing.T) {
	executed := false
	handler := Handler{
		Executor: executorFunc(func(context.Context, string) (reviewexecute.Result, error) {
			executed = true
			return reviewexecute.Result{}, nil
		}),
		Events: &eventRecorder{},
	}
	for _, payload := range []string{
		`{}`, `{"run_id":"run-1","extra":true}`, `{"run_id":" run-1"}`, `{"run_id":"run/1"}`, `{"run_id":"run-1"} null`,
	} {
		err := handler.Handle(context.Background(), reviewJob(payload))
		if err == nil || !worker.IsPermanent(err) || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("payload=%s err=%v", payload, err)
		}
	}
	if executed {
		t.Fatal("executor called for malformed payload")
	}
}

func TestHandlerRecordsOnlySafeClassifiedFailures(t *testing.T) {
	testCases := []struct {
		name      string
		execution error
		kind      string
		code      string
		permanent bool
	}{
		{name: "engine", execution: fmtError(engine.ErrEngineExit), kind: sqlite.ReviewRunEventFailedRetryable, code: "engine_exit"},
		{name: "provider rate", execution: fmtError(&githubadapter.HTTPError{StatusCode: 429, Message: "token=never-store-this"}), kind: sqlite.ReviewRunEventFailedRetryable, code: "rate_limited"},
		{name: "stale", execution: fmtError(sqlite.ErrReviewRunExecutionTargetNotFound), kind: sqlite.ReviewRunEventFailedTerminal, code: "stale_evidence", permanent: true},
		{name: "invalid output", execution: errors.New("validate review engine assessment: raw stdout must not persist"), kind: sqlite.ReviewRunEventFailedTerminal, code: "validation_failed", permanent: true},
		{name: "coverage invalid", execution: errors.New("validate review engine assessment: assessment coverage is invalid"), kind: sqlite.ReviewRunEventFailedTerminal, code: "validation_failed", permanent: true},
		{name: "native provider", execution: errors.New("run review engine: native engine execution: exit status 1"), kind: sqlite.ReviewRunEventFailedTerminal, code: "configuration_invalid", permanent: true},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			events := &eventRecorder{}
			handler := Handler{
				Executor: executorFunc(func(context.Context, string) (reviewexecute.Result, error) {
					return reviewexecute.Result{}, test.execution
				}),
				Events: events,
				Now:    func() time.Time { return time.Unix(7, 0).UTC() },
			}
			err := handler.Handle(context.Background(), reviewJob(`{"run_id":"run-1"}`))
			if err == nil || worker.IsPermanent(err) != test.permanent {
				t.Fatalf("err=%v permanent=%t", err, worker.IsPermanent(err))
			}
			if strings.Contains(err.Error(), "token=") || strings.Contains(err.Error(), "stdout") {
				t.Fatalf("unsafe returned error: %v", err)
			}
			if test.name == "native provider" && !strings.Contains(err.Error(), "run its status command") {
				t.Fatalf("missing operator guidance: %v", err)
			}
			if test.name == "coverage invalid" && !strings.Contains(err.Error(), "assessment coverage is invalid") {
				t.Fatalf("missing validation diagnostic: %v", err)
			}
			if len(events.inputs) != 1 {
				t.Fatalf("events=%+v", events.inputs)
			}
			input := events.inputs[0]
			if input.RunID != "run-1" || input.EventKind != test.kind || input.DiagnosticCode != test.code || !input.OccurredAt.Equal(time.Unix(7, 0).UTC()) {
				t.Fatalf("input=%+v", input)
			}
		})
	}
}

func TestHandlerRetriesLifecycleWriteFailure(t *testing.T) {
	handler := Handler{
		Executor: executorFunc(func(context.Context, string) (reviewexecute.Result, error) {
			return reviewexecute.Result{}, engine.ErrEngineExit
		}),
		Events: &eventRecorder{err: errors.New("database offline")},
	}
	err := handler.Handle(context.Background(), reviewJob(`{"run_id":"run-1"}`))
	if err == nil || worker.IsPermanent(err) || err.Error() != "review run lifecycle recording failed" {
		t.Fatalf("err=%v", err)
	}
}

func TestHandlerEmitsReviewLifecycleNotifications(t *testing.T) {
	notifications := &notificationEventRecorder{}
	handler := Handler{
		Executor: executorFunc(func(context.Context, string) (reviewexecute.Result, error) {
			return reviewexecute.Result{}, nil
		}),
		Events:           &eventRecorder{},
		Notifications:    notifications,
		LifecycleTargets: lifecycleTargetLoader{target: sqlite.ReviewRunExecutionTarget{Owner: "acme", Repository: "widgets", Number: 42, Title: "Fix widget", Author: "octocat"}},
	}
	if err := handler.Handle(context.Background(), reviewJob(`{"run_id":"run-1"}`)); err != nil {
		t.Fatal(err)
	}
	if len(notifications.events) != 2 || notifications.events[0].EventType != "review.started" || notifications.events[1].EventType != "review.completed" {
		t.Fatalf("events=%+v", notifications.events)
	}
	for _, event := range notifications.events {
		if event.AggregateType != "review_run" || event.AggregateID != "run-1" || len(event.Payload) == 0 {
			t.Fatalf("event=%+v", event)
		}
		if !bytes.Contains(event.Payload, []byte(`"title":"Fix widget"`)) || !bytes.Contains(event.Payload, []byte(`"author":"octocat"`)) {
			t.Fatalf("event payload=%s", event.Payload)
		}
	}
}

type lifecycleTargetLoader struct {
	target sqlite.ReviewRunExecutionTarget
	err    error
}

func (l lifecycleTargetLoader) LoadReviewRunExecutionTarget(context.Context, string) (sqlite.ReviewRunExecutionTarget, error) {
	return l.target, l.err
}

func reviewJob(payload string) sqlite.Job {
	return sqlite.Job{Kind: ExecuteJobKind, Payload: []byte(payload)}
}

type executorFunc func(context.Context, string) (reviewexecute.Result, error)

func (f executorFunc) Execute(ctx context.Context, runID string) (reviewexecute.Result, error) {
	return f(ctx, runID)
}

type eventRecorder struct {
	inputs []sqlite.AppendReviewRunEventInput
	err    error
}

type notificationEventRecorder struct{ events []sqlite.DomainEventInput }

func (r *notificationEventRecorder) AppendEventWithOutbox(_ context.Context, event sqlite.DomainEventInput, outbox []sqlite.OutboxInput) (sqlite.AppendedEvent, error) {
	if len(outbox) != 1 || outbox[0].Topic != "notification.dispatch.v1" {
		return sqlite.AppendedEvent{}, errors.New("unexpected notification outbox")
	}
	r.events = append(r.events, event)
	return sqlite.AppendedEvent{EventID: event.ID}, nil
}

func (r *eventRecorder) AppendReviewRunEvent(_ context.Context, input sqlite.AppendReviewRunEventInput) (sqlite.AppendReviewRunEventResult, error) {
	r.inputs = append(r.inputs, input)
	return sqlite.AppendReviewRunEventResult{}, r.err
}

func fmtError(err error) error { return errors.Join(errors.New("review execution failed"), err) }

type automaticPolicyStore struct {
	target          sqlite.AutomaticWatchRuleTarget
	recorded        []sqlite.RecordPolicyEvaluationInput
	events          []sqlite.DomainEventInput
	publicationJSON []byte
}

func (s *automaticPolicyStore) LoadAutomaticWatchRuleTarget(context.Context, string, string) (sqlite.AutomaticWatchRuleTarget, error) {
	return s.target, nil
}

func (s *automaticPolicyStore) LoadPolicyEvaluationTarget(context.Context, string) (sqlite.PolicyEvaluationTarget, error) {
	return automaticPolicyTarget(), nil
}

func (s *automaticPolicyStore) LoadActivePolicyRule(context.Context, string, string) (sqlite.ActivePolicyRule, error) {
	publicationJSON := s.publicationJSON
	if len(publicationJSON) == 0 {
		publicationJSON = []byte(`{}`)
	}
	return sqlite.ActivePolicyRule{
		PolicySetID: "policy-set-1", RuleID: "rule-1", VersionID: "rule-version-1", RuleKey: "rule-key-1",
		ProfileID: "profile-1", ProfileVersionID: "profile-version-1", PublicationJSON: publicationJSON,
	}, nil
}

func (s *automaticPolicyStore) RecordPolicyEvaluation(_ context.Context, input sqlite.RecordPolicyEvaluationInput) (sqlite.RecordPolicyEvaluationResult, error) {
	s.recorded = append(s.recorded, input)
	return sqlite.RecordPolicyEvaluationResult{PolicyEvaluationID: "evaluation-1", ProposalID: "proposal-1", ProposalRevisionID: "proposal-revision-1", Created: true}, nil
}

type automaticPublisher struct {
	revisionID string
	err        error
}

func (p *automaticPublisher) PublishAutomaticApproval(_ context.Context, revisionID string) error {
	p.revisionID = revisionID
	return p.err
}

func (s *automaticPolicyStore) AppendEventWithOutbox(_ context.Context, event sqlite.DomainEventInput, _ []sqlite.OutboxInput) (sqlite.AppendedEvent, error) {
	s.events = append(s.events, event)
	return sqlite.AppendedEvent{EventID: event.ID}, nil
}

func automaticWatchTarget(triggerKind, profileID, profileVersionID string) sqlite.AutomaticWatchRuleTarget {
	return sqlite.AutomaticWatchRuleTarget{
		ConnectionID: "connection-1", PullRequestID: "pr-1",
		Facts: watchrule.Facts{RepositoryID: 1},
		Rules: []sqlite.AutomaticWatchRule{{
			PolicySetID: "policy-set-1", RuleID: "rule-1", VersionID: "rule-version-1", RuleKey: "rule-key-1",
			Priority: 0, TriggerKind: triggerKind, ProfileID: profileID, ProfileVersionID: profileVersionID, MatchJSON: []byte(`{}`),
		}},
	}
}

func automaticExecutionResult(triggerKind string) reviewexecute.Result {
	return reviewexecute.Result{
		Target: sqlite.ReviewRunExecutionTarget{
			TriggerKind: triggerKind, ConnectionID: "connection-1", PullRequestID: "pr-1",
			Profile: sqlite.ReviewExecutionProfile{ProfileID: "profile-1", ProfileVersionID: "profile-version-1"},
		},
		Recorded: sqlite.RecordAssessmentResult{AssessmentID: "assessment-1"},
	}
}

func automaticPolicyTarget() sqlite.PolicyEvaluationTarget {
	coverage := assessment.Coverage{Status: assessment.CoverageComplete, ChangedFilesTotal: 1, ReviewedFiles: 1, Omitted: []assessment.Omission{}}
	return sqlite.PolicyEvaluationTarget{
		AssessmentID: "assessment-1", ProfileID: "profile-1", ProfileVersionID: "profile-version-1",
		Assessment: assessment.Result{Assessment: assessment.Assessment{
			Version: assessment.Version1, Verdict: assessment.VerdictPass, Summary: "no concerns", Confidence: assessment.ConfidenceHigh,
			Limitations: []string{}, Coverage: coverage, Findings: []assessment.Finding{},
		}, ValidationWarnings: []assessment.ValidationWarning{}},
		Facts: sqlite.PolicyEvaluationFacts{EvidenceCurrent: true, Coverage: coverage},
	}
}
