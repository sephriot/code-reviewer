package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestLoadAutomaticWatchRuleTargetReturnsCurrentFactsAndRules(t *testing.T) {
	ctx := context.Background()
	store, _ := seedPolicyRecordFixture(t, ctx)

	target, err := store.LoadAutomaticWatchRuleTarget(ctx, "connection-1", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if target.ConnectionID != "connection-1" || target.PullRequestID != "pr-1" ||
		target.Canonical.ObservationID == "" || target.Canonical.RevisionID == "" ||
		target.Facts.RepositoryID != 1042 || target.Facts.RepositoryFullName != "owner/repo-1" ||
		target.Facts.AuthorLogin != "author" || target.Facts.IsDraft || target.Facts.State != "open" ||
		target.Facts.BaseRef != "main" || !reflect.DeepEqual(target.Facts.Labels, []string{}) ||
		!reflect.DeepEqual(target.Facts.Relationships, []string{}) {
		t.Fatalf("target facts = %+v", target)
	}
	if len(target.Rules) != 1 {
		t.Fatalf("rules = %+v", target.Rules)
	}
	rule := target.Rules[0]
	if rule.PolicySetID != "policy-set-1" || rule.RuleID != "rule-1" || rule.VersionID != "rule-version-1" ||
		rule.RuleKey != "assigned-default" || rule.Priority != 0 || rule.TriggerKind != "automatic" ||
		rule.ProfileID != "profile-1" || rule.ProfileVersionID != "profile-version-1" ||
		string(rule.MatchJSON) != "{}" || string(rule.ReviewJSON) != "{}" {
		t.Fatalf("rule = %+v", rule)
	}
}

func TestLoadAutomaticWatchRuleTargetRejectsStaleOrInvalidStoredRules(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	profile := mustCreatePolicyTestProfile(t, ctx, store)

	if _, err := store.CreatePolicySetGeneration(ctx, PolicySetGenerationInput{
		Generation: 1,
		Rules: []WatchRuleVersionInput{{
			RuleKey: "assigned-default", Enabled: true, Priority: 0,
			TriggerKind: "automatic", ExternalActionPolicy: "require_confirmation",
			ProfileID: profile.ProfileID, ProfileVersionID: profile.VersionID,
			MatchJSON: []byte(`{"unknown":true}`), ReviewJSON: []byte(`{}`), PublicationJSON: []byte(`{}`),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadAutomaticWatchRuleTarget(ctx, "connection-1", "pr-1"); err == nil {
		t.Fatal("invalid watch rule match accepted")
	}

	store, _ = seedCurrentCanonicalReviewTarget(t, ctx)
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadAutomaticWatchRuleTarget(ctx, "connection-1", "pr-1"); !errors.Is(err, ErrAutomaticWatchRuleTargetNotFound) {
		t.Fatalf("stale target error = %v", err)
	}
}

func TestDecodeAutomaticWatchRuleFactSetRejectsMalformedStoredFacts(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`[1]`), []byte(`null`), []byte(`["x"] trailing`)} {
		if _, err := decodeAutomaticWatchRuleFactSet(raw); err == nil {
			t.Fatalf("invalid fact set accepted: %q", raw)
		}
	}
}
