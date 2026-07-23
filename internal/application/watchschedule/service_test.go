package watchschedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/watchrule"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

func TestScheduleQueuesOnlySelectedAutomaticRule(t *testing.T) {
	store := &fakeStore{target: fixtureTarget([]sqlite.AutomaticWatchRule{
		{RuleID: "later", VersionID: "later-v", Priority: 20, TriggerKind: "automatic", ProfileID: "p", ProfileVersionID: "pv", MatchJSON: []byte(`{}`)},
		{RuleID: "first", VersionID: "first-v", Priority: 10, TriggerKind: "automatic", ProfileID: "p", ProfileVersionID: "pv", MatchJSON: []byte(`{"labels":["security"]}`)},
	})}
	result, err := (Service{Store: store}).Schedule(context.Background(), Request{ConnectionID: "c", PullRequestID: "pr", EngineKind: "cli", EngineConfigJSON: []byte(`{}`), AccessMode: "diff_only", RequestedAt: time.Unix(10, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Matched || result.RuleID != "first" || result.Queued == nil || store.input.TriggerKind != "automatic" || store.input.ProfileVersionID != "pv" || store.input.TriggerSHA256 == "" {
		t.Fatalf("result=%+v input=%+v", result, store.input)
	}
	if again := automaticTriggerSHA(store.target, store.target.Rules[1]); again != store.input.TriggerSHA256 {
		t.Fatal("trigger is not deterministic")
	}
}

func TestScheduleDoesNotQueueManualOrNoMatch(t *testing.T) {
	for _, rule := range []sqlite.AutomaticWatchRule{
		{RuleID: "manual", VersionID: "manual-v", Priority: 1, TriggerKind: "manual", ProfileID: "p", ProfileVersionID: "pv", MatchJSON: []byte(`{}`)},
		{RuleID: "none", VersionID: "none-v", Priority: 1, TriggerKind: "automatic", ProfileID: "p", ProfileVersionID: "pv", MatchJSON: []byte(`{"labels":["missing"]}`)},
	} {
		store := &fakeStore{target: fixtureTarget([]sqlite.AutomaticWatchRule{rule})}
		result, err := (Service{Store: store}).Schedule(context.Background(), validRequest())
		if err != nil {
			t.Fatal(err)
		}
		if store.called || result.Queued != nil {
			t.Fatalf("rule %q queued: %+v", rule.RuleID, result)
		}
	}
}

func TestScheduleForceCreatesDistinctOperatorRetry(t *testing.T) {
	store := &fakeStore{target: fixtureTarget([]sqlite.AutomaticWatchRule{{RuleID: "rule", VersionID: "rule-v", Priority: 1, TriggerKind: "automatic", ProfileID: "p", ProfileVersionID: "pv", MatchJSON: []byte(`{}`)}})}
	request := validRequest()
	request.RequestedAt = time.Unix(20, 0)
	if _, err := (Service{Store: store}).Schedule(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	normalTrigger := store.input.TriggerSHA256
	request.Force = true
	if _, err := (Service{Store: store}).Schedule(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if store.input.TriggerSHA256 == normalTrigger || store.input.IdempotencyKey != "watch-rule:v1:"+store.input.TriggerSHA256 {
		t.Fatalf("forced input=%+v normal=%q", store.input, normalTrigger)
	}
}

func TestScheduleFailsClosed(t *testing.T) {
	store := &fakeStore{target: fixtureTarget([]sqlite.AutomaticWatchRule{{RuleID: "bad", VersionID: "bad-v", Priority: 1, TriggerKind: "automatic", ProfileID: "", MatchJSON: []byte(`{}`)}})}
	if _, err := (Service{Store: store}).Schedule(context.Background(), validRequest()); err == nil || store.called {
		t.Fatalf("error=%v called=%v", err, store.called)
	}
	store.loadErr = errors.New("stale")
	if _, err := (Service{Store: store}).Schedule(context.Background(), validRequest()); err == nil {
		t.Fatal("load error accepted")
	}
}

type fakeStore struct {
	target  sqlite.AutomaticWatchRuleTarget
	input   sqlite.PrepareReviewRunInput
	called  bool
	loadErr error
}

func (f *fakeStore) LoadAutomaticWatchRuleTarget(_ context.Context, _, _ string) (sqlite.AutomaticWatchRuleTarget, error) {
	return f.target, f.loadErr
}
func (f *fakeStore) QueueReviewRun(_ context.Context, input sqlite.PrepareReviewRunInput) (sqlite.QueueReviewRunResult, error) {
	f.called = true
	f.input = input
	return sqlite.QueueReviewRunResult{IntentID: "i", RunID: "r", Created: true}, nil
}
func validRequest() Request {
	return Request{ConnectionID: "c", PullRequestID: "pr", EngineKind: "cli", EngineConfigJSON: []byte(`{}`), AccessMode: "diff_only"}
}
func fixtureTarget(rules []sqlite.AutomaticWatchRule) sqlite.AutomaticWatchRuleTarget {
	return sqlite.AutomaticWatchRuleTarget{ConnectionID: "c", PullRequestID: "pr", Canonical: sqlite.CanonicalReviewTarget{RevisionID: "rev", ObservationID: "obs"}, Facts: watchrule.Facts{RepositoryID: 1, RepositoryFullName: "o/r", AuthorLogin: "a", Labels: []string{"security"}, Relationships: []string{}, State: "open", BaseRef: "main"}, Rules: rules}
}
