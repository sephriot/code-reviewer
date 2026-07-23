package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRecordPolicyEvaluationPersistsBoundProposal(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)

	result, err := store.RecordPolicyEvaluation(ctx, policyRecordInput(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.PolicyEvaluationID == "" || result.ProposalID == "" || result.ProposalRevisionID == "" {
		t.Fatalf("result = %+v", result)
	}
	for _, table := range []string{"policy_evaluations", "proposals", "proposal_revisions"} {
		assertTableCount(t, ctx, store.db, table, 1)
	}
	for _, table := range []string{"decisions", "publication_effects", "publication_attempts", "jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}

	var disposition, inputJSON, overrides, body, inlineJSON string
	if err := store.db.QueryRowContext(ctx, `
SELECT evaluation.disposition, evaluation.input_json, evaluation.safety_overrides_json,
       revision.body, revision.inline_comments_json
FROM policy_evaluations AS evaluation
JOIN proposals AS proposal ON proposal.policy_evaluation_id = evaluation.id
JOIN proposal_revisions AS revision ON revision.proposal_id = proposal.id
WHERE evaluation.id = ?`, result.PolicyEvaluationID).Scan(&disposition, &inputJSON, &overrides, &body, &inlineJSON); err != nil {
		t.Fatal(err)
	}
	if disposition != string(PolicyDispositionProposeChanges) || inputJSON != `{"assessment":"concerns","source":"v1"}` ||
		overrides != `["force_confirmation"]` || body != "Request nil guard.\n" || inlineJSON != `[{"path":"internal/example.go"}]` {
		t.Fatalf("stored policy record = %q %q %q %q %q", disposition, inputJSON, overrides, body, inlineJSON)
	}
}

func TestRecordPolicyEvaluationIdempotenceAndConflict(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)
	input := policyRecordInput(fixture)
	first, err := store.RecordPolicyEvaluation(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input.CreatedAt = input.CreatedAt.Add(time.Hour)
	second, err := store.RecordPolicyEvaluation(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || first.PolicyEvaluationID != second.PolicyEvaluationID ||
		first.ProposalID != second.ProposalID || first.ProposalRevisionID != second.ProposalRevisionID {
		t.Fatalf("results = %+v / %+v", first, second)
	}

	input.RenderedBody = "Changed content."
	_, err = store.RecordPolicyEvaluation(ctx, input)
	if !errors.Is(err, ErrPolicyEvaluationConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	for _, table := range []string{"policy_evaluations", "proposals", "proposal_revisions"} {
		assertTableCount(t, ctx, store.db, table, 1)
	}
}

func TestRecordPolicyEvaluationDoesNotDuplicateProposalForSameEvidenceAndRule(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)
	first, err := store.RecordPolicyEvaluation(ctx, policyRecordInput(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.ProposalID == "" || first.ProposalRevisionID == "" {
		t.Fatalf("first evaluation = %+v", first)
	}

	secondRun := prepareInboxRun(t, ctx, store, "repeat-policy-evidence")
	secondAssessment, err := store.RecordAssessment(ctx, RecordAssessmentInput{
		RunID: secondRun.RunID, Result: testValidatedAssessmentResult(t), RecordedAt: time.Unix(61, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondInput := policyRecordInput(policyRecordFixture{assessmentID: secondAssessment.AssessmentID})
	secondInput.Disposition = PolicyDispositionProposeComment
	secondInput.RenderedBody = "A later model result selected a comment."
	secondInput.InlineCommentsJSON = []byte(`[]`)
	second, err := store.RecordPolicyEvaluation(ctx, secondInput)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Created || second.ProposalID != "" || second.ProposalRevisionID != "" {
		t.Fatalf("repeat evaluation = %+v", second)
	}
	assertTableCount(t, ctx, store.db, "policy_evaluations", 2)
	assertTableCount(t, ctx, store.db, "proposals", 1)
	assertTableCount(t, ctx, store.db, "proposal_revisions", 1)
}

func TestRecordPolicyEvaluationRequiresCurrentEvidenceAndResolvedRuleProfile(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)
	input := policyRecordInput(fixture)
	input.ProfileVersionID = "different-version"
	if _, err := store.RecordPolicyEvaluation(ctx, input); err == nil || !strings.Contains(err.Error(), "profile differs") {
		t.Fatalf("profile mismatch = %v", err)
	}
	input = policyRecordInput(fixture)
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	_, err := store.RecordPolicyEvaluation(ctx, input)
	if !errors.Is(err, ErrCanonicalReviewTargetNotFound) {
		t.Fatalf("stale evidence error = %v", err)
	}
	for _, table := range []string{"policy_evaluations", "proposals", "proposal_revisions"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func TestRecordPolicyEvaluationNoProposalForNoExternalAction(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)
	input := policyRecordInput(fixture)
	input.Disposition = PolicyDispositionNoExternalAction
	input.RenderedBody = ""
	input.InlineCommentsJSON = []byte(`[]`)
	result, err := store.RecordPolicyEvaluation(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.ProposalID != "" || result.ProposalRevisionID != "" {
		t.Fatalf("result = %+v", result)
	}
	assertTableCount(t, ctx, store.db, "policy_evaluations", 1)
	for _, table := range []string{"proposals", "proposal_revisions", "decisions", "publication_effects", "jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func TestRecordPolicyEvaluationCreatesPolicyOwnedAutoApprovalProposal(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)
	input := policyRecordInput(fixture)
	input.Disposition = PolicyDispositionAutoPublishApproval
	input.RenderedBody = ""
	input.InlineCommentsJSON = []byte(`[]`)

	result, err := store.RecordPolicyEvaluation(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.ProposalID == "" || result.ProposalRevisionID == "" {
		t.Fatalf("result = %+v", result)
	}
	var kind, body string
	if err := store.db.QueryRowContext(ctx, `
SELECT proposal.proposal_kind, revision.body
FROM proposals AS proposal
JOIN proposal_revisions AS revision ON revision.proposal_id = proposal.id
WHERE proposal.id = ?`, result.ProposalID).Scan(&kind, &body); err != nil {
		t.Fatal(err)
	}
	if kind != "approval" || body != "" {
		t.Fatalf("auto approval proposal = kind=%q body=%q", kind, body)
	}
	var decision, actorKind, actorID, reason string
	if err := store.db.QueryRowContext(ctx, `SELECT decision, actor_kind, actor_id, reason FROM decisions WHERE proposal_revision_id = ?`, result.ProposalRevisionID).Scan(&decision, &actorKind, &actorID, &reason); err != nil {
		t.Fatal(err)
	}
	if decision != "approve" || actorKind != "policy" || actorID != "policy:rule-version-1" || reason != "automatic approval authorized by immutable policy" {
		t.Fatalf("auto approval decision = %q,%q,%q,%q", decision, actorKind, actorID, reason)
	}

	repeatRun := prepareInboxRun(t, ctx, store, "repeat-auto-policy-evidence")
	repeatAssessment, err := store.RecordAssessment(ctx, RecordAssessmentInput{
		RunID: repeatRun.RunID, Result: testValidatedAssessmentResult(t), RecordedAt: time.Unix(61, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	input.AssessmentID = repeatAssessment.AssessmentID
	repeat, err := store.RecordPolicyEvaluation(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !repeat.Created || repeat.ProposalID != "" || repeat.ProposalRevisionID != "" {
		t.Fatalf("repeat auto evaluation = %+v", repeat)
	}
	assertTableCount(t, ctx, store.db, "proposals", 1)
	assertTableCount(t, ctx, store.db, "decisions", 1)
}

type policyRecordFixture struct {
	assessmentID string
}

func seedPolicyRecordFixture(t *testing.T, ctx context.Context) (*Store, policyRecordFixture) {
	t.Helper()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	seedReviewProfileVersion(t, ctx, store, "profile-1", "profile-version-1")
	prepared, err := store.PrepareReviewRun(ctx, testPrepareReviewRunInput())
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := store.RecordAssessment(ctx, RecordAssessmentInput{
		RunID: prepared.RunID, Result: testValidatedAssessmentResult(t), RecordedAt: time.Unix(40, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO policy_sets(id, generation, content_sha256, created_at_us)
VALUES ('policy-set-1', 1, '` + policyDigestOne + `', 50)`,
		`INSERT INTO watch_rules(id, rule_key, enabled, current_version_id, created_at_us, updated_at_us)
VALUES ('rule-1', 'assigned-default', 1, NULL, 50, 50)`,
		`INSERT INTO watch_rule_versions(
 id, rule_id, policy_set_id, version, priority, trigger_kind, external_action_policy,
 profile_id, profile_version_id, match_json, review_json, publication_json,
 content_sha256, created_at_us)
VALUES ('rule-version-1', 'rule-1', 'policy-set-1', 1, 0, 'automatic',
 'require_confirmation', 'profile-1', 'profile-version-1', '{}', '{}', '{}',
 '` + policyDigestTwo + `', 50)`,
		`UPDATE watch_rules SET current_version_id = 'rule-version-1', updated_at_us = 51
WHERE id = 'rule-1'`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed policy fixture: %v\n%s", err, statement)
		}
	}
	return store, policyRecordFixture{assessmentID: recorded.AssessmentID}
}

func policyRecordInput(fixture policyRecordFixture) RecordPolicyEvaluationInput {
	return RecordPolicyEvaluationInput{
		AssessmentID: fixture.assessmentID,
		PolicySetID:  "policy-set-1", MatchedRuleID: "rule-1", MatchedRuleVersionID: "rule-version-1",
		ProfileID: "profile-1", ProfileVersionID: "profile-version-1",
		Disposition:         PolicyDispositionProposeChanges,
		InputSnapshotJSON:   []byte(` { "source" : "v1", "assessment" : "concerns" } `),
		SafetyOverridesJSON: []byte(` [ "force_confirmation" ] `),
		RenderedBody:        "Request nil guard.\r\n",
		InlineCommentsJSON:  []byte(`[{"path":"internal/example.go"}]`),
		CreatedAt:           time.Unix(60, 0).UTC(),
	}
}
