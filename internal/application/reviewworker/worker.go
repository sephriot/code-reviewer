// Package reviewworker executes bounded, prepared review runs from durable jobs.
package reviewworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/adapters/engine"
	githubadapter "github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/application/policy"
	"github.com/sephriot/code-reviewer/internal/application/policyevaluate"
	"github.com/sephriot/code-reviewer/internal/application/reviewexecute"
	"github.com/sephriot/code-reviewer/internal/application/watchrule"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

// ExecuteJobKind is the durable job type for one prepared review run.
const ExecuteJobKind = "review.execute.v1"

const maxRunIDBytes = 256

// Executor is the narrow review execution boundary used by Handler. The app
// constructs reviewexecute.Service with its read-only reader and engine.
type Executor interface {
	Execute(context.Context, string) (reviewexecute.Result, error)
}

// RunEvents records safe, bounded lifecycle outcomes for a review run.
type RunEvents interface {
	AppendReviewRunEvent(context.Context, sqlite.AppendReviewRunEventInput) (sqlite.AppendReviewRunEventResult, error)
}

// AutomaticPolicyStore supplies the bounded current-rule and immutable policy
// evaluation capabilities used only after an automatic review succeeds.
type AutomaticPolicyStore interface {
	policyevaluate.Reader
	policyevaluate.Recorder
	policyevaluate.EventAppender
	LoadAutomaticWatchRuleTarget(context.Context, string, string) (sqlite.AutomaticWatchRuleTarget, error)
}

// AutomaticPublication creates and schedules only a policy-authorized
// automatic approval. Its implementation must still honor global mode.
type AutomaticPublication interface {
	PublishAutomaticApproval(context.Context, string) error
}

// Handler executes a single prepared review run. It has no GitHub publication
// dependency and stores only fixed diagnostic codes for failures.
type Handler struct {
	Executor             Executor
	Events               RunEvents
	AutomaticPolicyStore AutomaticPolicyStore
	AutomaticPublication AutomaticPublication
	Now                  func() time.Time
}

// Handle implements worker.Handler.
func (h Handler) Handle(ctx context.Context, job sqlite.Job) error {
	if job.Kind != ExecuteJobKind {
		return worker.Permanent(errors.New("unexpected review execution job kind"))
	}
	runID, err := parseJobPayload(job.Payload)
	if err != nil {
		return worker.Permanent(fmt.Errorf("malformed review execution job payload: %w", err))
	}
	if h.Executor == nil || h.Events == nil {
		return h.recordFailure(ctx, runID, failureTerminal, "configuration_invalid", errors.New("review execution dependencies are unavailable"))
	}

	execution, err := h.Executor.Execute(ctx, runID)
	if err == nil {
		if err := h.evaluateAutomaticPolicy(ctx, execution); err != nil {
			// The assessment is already durably successful. Retrying this execution
			// job would only try to execute an already-completed run, so fail this
			// job permanently and leave publication unavailable.
			return worker.Permanent(err)
		}
		return nil
	}
	result := classifyFailure(err)
	return h.recordFailure(ctx, runID, result.kind, result.code, err)
}

func (h Handler) evaluateAutomaticPolicy(ctx context.Context, execution reviewexecute.Result) error {
	if execution.Target.TriggerKind != "automatic" || h.AutomaticPolicyStore == nil {
		return nil
	}
	connectionID := strings.TrimSpace(execution.Target.ConnectionID)
	pullRequestID := strings.TrimSpace(execution.Target.PullRequestID)
	if connectionID == "" || pullRequestID == "" || execution.Recorded.AssessmentID == "" {
		return errors.New("successful automatic review lacks policy evaluation identity")
	}
	target, err := h.AutomaticPolicyStore.LoadAutomaticWatchRuleTarget(ctx, connectionID, pullRequestID)
	if err != nil {
		return fmt.Errorf("load automatic policy target: %w", err)
	}
	rules := make([]watchrule.Rule, len(target.Rules))
	byVersion := make(map[string]sqlite.AutomaticWatchRule, len(target.Rules))
	for index, rule := range target.Rules {
		rules[index] = watchrule.Rule{ID: rule.VersionID, Enabled: true, Priority: rule.Priority, MatchJSON: rule.MatchJSON}
		byVersion[rule.VersionID] = rule
	}
	selection, err := watchrule.Select(target.Facts, rules)
	if err != nil {
		return fmt.Errorf("select automatic policy rule: %w", err)
	}
	if !selection.Found {
		return nil
	}
	rule := byVersion[selection.Rule.ID]
	if rule.TriggerKind != "automatic" || rule.ProfileID != execution.Target.Profile.ProfileID ||
		rule.ProfileVersionID != execution.Target.Profile.ProfileVersionID {
		return nil
	}
	evaluation, err := (policyevaluate.Service{Reader: h.AutomaticPolicyStore, Recorder: h.AutomaticPolicyStore, Events: h.AutomaticPolicyStore, Now: h.Now}).Evaluate(ctx, policyevaluate.Request{
		AssessmentID: execution.Recorded.AssessmentID, RuleKey: rule.RuleKey, RuleVersionID: rule.VersionID,
	})
	if err != nil {
		return fmt.Errorf("evaluate automatic policy: %w", err)
	}
	if evaluation.Outcome.Disposition == policy.DispositionAutoPublishApproval && evaluation.Evaluation.ProposalRevisionID != "" && h.AutomaticPublication != nil {
		if err := h.AutomaticPublication.PublishAutomaticApproval(ctx, evaluation.Evaluation.ProposalRevisionID); err != nil {
			return fmt.Errorf("publish automatic approval: %w", err)
		}
	}
	return nil
}

type failureKind string

const (
	failureRetryable failureKind = "retryable"
	failureTerminal  failureKind = "terminal"
)

type failure struct {
	kind failureKind
	code string
}

func (h Handler) recordFailure(ctx context.Context, runID string, kind failureKind, code string, cause error) error {
	if h.Events == nil {
		return worker.Permanent(errors.New("review run event store is required"))
	}
	eventKind := sqlite.ReviewRunEventFailedTerminal
	if kind == failureRetryable {
		eventKind = sqlite.ReviewRunEventFailedRetryable
	}
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}
	_, err := h.Events.AppendReviewRunEvent(ctx, sqlite.AppendReviewRunEventInput{
		RunID: runID, EventKind: eventKind, DiagnosticCode: code, OccurredAt: now,
	})
	if err != nil && !errors.Is(err, sqlite.ErrReviewRunSucceeded) {
		// A lifecycle-write failure must be retried: otherwise a later worker
		// could execute the same run without its durable terminal outcome.
		return errors.New("review run lifecycle recording failed")
	}
	if kind == failureTerminal {
		return worker.Permanent(fmt.Errorf("review execution failed: %s: %s", code, safeFailureSummary(cause)))
	}
	return fmt.Errorf("review execution failed: %s: %s", code, safeFailureSummary(cause))
}

// safeFailureSummary keeps provider stderr, request content, and environment
// details out of durable job errors while giving operators enough direction to
// repair a local CLI setup.
func safeFailureSummary(cause error) string {
	if cause == nil {
		return "unknown review failure"
	}
	message := cause.Error()
	switch {
	case strings.Contains(message, "native engine execution"):
		return "native provider process failed; run its status command and confirm its login"
	case strings.Contains(message, "native engine output"):
		return "native provider did not return a structured assessment"
	case strings.Contains(message, "sandbox"):
		return "native provider sandbox configuration blocked execution"
	case strings.Contains(message, "validate review engine assessment"):
		return "provider assessment failed validation"
	case strings.Contains(message, "review execution dependencies"):
		return "review execution dependencies are unavailable"
	default:
		return "review execution failed"
	}
}

func classifyFailure(err error) failure {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return failure{kind: failureRetryable, code: "timeout"}
	}
	if errors.Is(err, engine.ErrEngineExit) {
		return failure{kind: failureRetryable, code: "engine_exit"}
	}
	if errors.Is(err, engine.ErrOutputLimit) {
		return failure{kind: failureTerminal, code: "engine_protocol_invalid"}
	}
	var providerErr *githubadapter.HTTPError
	if errors.As(err, &providerErr) {
		if providerErr.StatusCode == 429 {
			return failure{kind: failureRetryable, code: "rate_limited"}
		}
		return failure{kind: failureRetryable, code: "transport_unavailable"}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		if networkErr.Timeout() {
			return failure{kind: failureRetryable, code: "timeout"}
		}
		return failure{kind: failureRetryable, code: "transport_unavailable"}
	}
	if errors.Is(err, sqlite.ErrReviewRunExecutionTargetNotFound) ||
		strings.Contains(err.Error(), "rebuild review evidence") ||
		strings.Contains(err.Error(), "does not match requested run") {
		return failure{kind: failureTerminal, code: "stale_evidence"}
	}
	if strings.Contains(err.Error(), "validate review engine assessment") {
		return failure{kind: failureTerminal, code: "validation_failed"}
	}
	if strings.Contains(err.Error(), "review execution dependencies are required") ||
		strings.Contains(err.Error(), "review execution run ID is required") ||
		strings.Contains(err.Error(), "review execution context is required") {
		return failure{kind: failureTerminal, code: "configuration_invalid"}
	}
	if strings.Contains(err.Error(), "native engine") || strings.Contains(err.Error(), "sandbox") {
		return failure{kind: failureTerminal, code: "configuration_invalid"}
	}
	return failure{kind: failureRetryable, code: "internal_error"}
}

type jobPayload struct {
	RunID string `json:"run_id"`
}

func parseJobPayload(raw []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload jobPayload
	if err := decoder.Decode(&payload); err != nil {
		return "", errors.New("must be a single supported JSON object")
	}
	if err := requireEOF(decoder); err != nil {
		return "", errors.New("must be a single JSON object")
	}
	if payload.RunID == "" || payload.RunID != strings.TrimSpace(payload.RunID) ||
		len(payload.RunID) > maxRunIDBytes || !validRunID(payload.RunID) {
		return "", errors.New("run ID is invalid")
	}
	return payload.RunID, nil
}

func validRunID(value string) bool {
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("extra value")
	}
	return err
}

var _ worker.Handler = Handler{}
