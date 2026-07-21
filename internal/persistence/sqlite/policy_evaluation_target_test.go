package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestLoadActivePolicyRuleRequiresExactEnabledCurrentVersion(t *testing.T) {
	ctx := context.Background()
	store, _ := seedPolicyRecordFixture(t, ctx)

	rule, err := store.LoadActivePolicyRule(ctx, "ASSIGNED-DEFAULT", "rule-version-1")
	if err != nil {
		t.Fatal(err)
	}
	if rule.PolicySetID != "policy-set-1" || rule.RuleID != "rule-1" || rule.RuleKey != "assigned-default" ||
		rule.ProfileID != "profile-1" || rule.ProfileVersionID != "profile-version-1" || string(rule.PublicationJSON) != `{}` {
		t.Fatalf("rule = %+v", rule)
	}
	if _, err := store.LoadActivePolicyRule(ctx, "assigned-default", "wrong"); !errors.Is(err, ErrActivePolicyRuleNotFound) {
		t.Fatalf("wrong version error = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE watch_rules SET enabled = 0 WHERE id = 'rule-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadActivePolicyRule(ctx, "assigned-default", "rule-version-1"); !errors.Is(err, ErrActivePolicyRuleNotFound) {
		t.Fatalf("disabled rule error = %v", err)
	}
}

func TestLoadPolicyEvaluationTargetRequiresCurrentCompletedEvidence(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)

	target, err := store.LoadPolicyEvaluationTarget(ctx, fixture.assessmentID)
	if err != nil {
		t.Fatal(err)
	}
	if target.AssessmentID != fixture.assessmentID || target.ProfileID != "profile-1" ||
		target.Assessment.Assessment.Summary == "" || !target.Facts.EvidenceCurrent || target.Facts.Terminal || target.Facts.Draft ||
		!reflect.DeepEqual(target.Facts.Coverage, target.Assessment.Assessment.Coverage) {
		t.Fatalf("target = %+v", target)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadPolicyEvaluationTarget(ctx, fixture.assessmentID); !errors.Is(err, ErrPolicyEvaluationTargetNotFound) {
		t.Fatalf("stale target error = %v", err)
	}
}
