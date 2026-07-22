// Package watchschedule turns one selected automatic watch rule into durable
// review work. It deliberately has no GitHub or publication capability.
package watchschedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/watchrule"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

// Store is the narrow read/queue capability required for automatic review
// scheduling. QueueReviewRun creates the intent, run and durable job atomically.
type Store interface {
	LoadAutomaticWatchRuleTarget(context.Context, string, string) (sqlite.AutomaticWatchRuleTarget, error)
	QueueReviewRun(context.Context, sqlite.PrepareReviewRunInput) (sqlite.QueueReviewRunResult, error)
}

// Request names current local evidence and the trusted engine configuration.
// Engine configuration stays explicit bootstrap configuration, never rule JSON.
type Request struct {
	ConnectionID     string
	PullRequestID    string
	EngineKind       string
	EngineConfigJSON []byte
	AccessMode       string
	CorrelationID    string
	RequestedAt      time.Time
}

// Result distinguishes a selected rule from a queued automatic review.
type Result struct {
	Matched       bool
	RuleID        string
	RuleVersionID string
	TriggerKind   string
	Queued        *sqlite.QueueReviewRunResult
}

// Service selects ordered enabled rules and queues work only for automatic
// triggers. Manual, track-only and ignore rules are recorded by callers as
// policy selection facts later; this service intentionally performs no write.
type Service struct{ Store Store }

func (s Service) Schedule(ctx context.Context, request Request) (Result, error) {
	if s.Store == nil {
		return Result{}, errors.New("automatic watch-rule store is required")
	}
	if strings.TrimSpace(request.ConnectionID) == "" || strings.TrimSpace(request.PullRequestID) == "" ||
		strings.TrimSpace(request.EngineKind) == "" || strings.TrimSpace(request.AccessMode) == "" {
		return Result{}, errors.New("automatic watch-rule request is invalid")
	}
	target, err := s.Store.LoadAutomaticWatchRuleTarget(ctx, request.ConnectionID, request.PullRequestID)
	if err != nil {
		return Result{}, fmt.Errorf("load automatic watch-rule target: %w", err)
	}
	rules := make([]watchrule.Rule, len(target.Rules))
	byVersion := make(map[string]sqlite.AutomaticWatchRule, len(target.Rules))
	for index, rule := range target.Rules {
		rules[index] = watchrule.Rule{ID: rule.VersionID, Enabled: true, Priority: rule.Priority, MatchJSON: rule.MatchJSON}
		byVersion[rule.VersionID] = rule
	}
	selection, err := watchrule.Select(target.Facts, rules)
	if err != nil {
		return Result{}, fmt.Errorf("select automatic watch rule: %w", err)
	}
	if !selection.Found {
		return Result{}, nil
	}
	rule := byVersion[selection.Rule.ID]
	result := Result{Matched: true, RuleID: rule.RuleID, RuleVersionID: rule.VersionID, TriggerKind: rule.TriggerKind}
	if rule.TriggerKind != "automatic" {
		return result, nil
	}
	if rule.ProfileID == "" || rule.ProfileVersionID == "" {
		return Result{}, errors.New("automatic watch rule lacks review profile")
	}
	requestedAt := request.RequestedAt.UTC()
	if requestedAt.IsZero() {
		requestedAt = time.Now().UTC()
	}
	trigger := automaticTriggerSHA(target, rule)
	queued, err := s.Store.QueueReviewRun(ctx, sqlite.PrepareReviewRunInput{
		ConnectionID: target.ConnectionID, PullRequestID: target.PullRequestID,
		ProfileID: rule.ProfileID, ProfileVersionID: rule.ProfileVersionID,
		TriggerKind: "automatic", TriggerSHA256: trigger, CorrelationID: strings.TrimSpace(request.CorrelationID),
		IdempotencyKey: "watch-rule:v1:" + trigger, EngineKind: request.EngineKind,
		EngineConfigJSON: append([]byte(nil), request.EngineConfigJSON...), AccessMode: request.AccessMode,
		RequestedAt: requestedAt,
	})
	if err != nil {
		return Result{}, fmt.Errorf("queue automatic review: %w", err)
	}
	result.Queued = &queued
	return result, nil
}

func automaticTriggerSHA(target sqlite.AutomaticWatchRuleTarget, rule sqlite.AutomaticWatchRule) string {
	encoded, _ := json.Marshal(struct {
		Version, Connection, PullRequest, Revision, Observation, RuleVersion string
	}{"watch-rule-trigger:v1", target.ConnectionID, target.PullRequestID, target.Canonical.RevisionID, target.Canonical.ObservationID, rule.VersionID})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
