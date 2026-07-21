package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	policyDigestOne = "1111111111111111111111111111111111111111111111111111111111111111"
	policyDigestTwo = "2222222222222222222222222222222222222222222222222222222222222222"
	policyDigestTri = "3333333333333333333333333333333333333333333333333333333333333333"
	policyDigestFor = "4444444444444444444444444444444444444444444444444444444444444444"
)

func TestPolicyAndPublicationLedgerAnchorsImmutableAuthorization(t *testing.T) {
	ctx := context.Background()
	store, ids := seedPolicyPublicationChain(t, ctx)

	for _, table := range []string{
		"policy_sets", "watch_rule_versions", "policy_evaluations", "proposals",
		"proposal_revisions", "decisions", "publication_effects", "publication_attempts",
	} {
		if _, err := store.db.ExecContext(ctx, "DELETE FROM "+table); err == nil {
			t.Fatalf("deleting immutable %s was accepted", table)
		}
	}
	for _, statement := range []string{
		"UPDATE policy_sets SET generation = 2 WHERE id = 'policy-set-1'",
		"UPDATE watch_rule_versions SET priority = 1 WHERE id = 'rule-version-1'",
		"UPDATE policy_evaluations SET disposition = 'propose_comment' WHERE id = 'policy-evaluation-1'",
		"UPDATE proposals SET proposal_kind = 'comment' WHERE id = 'proposal-1'",
		"UPDATE proposal_revisions SET body = 'edited' WHERE id = 'proposal-revision-1'",
		"UPDATE decisions SET decision = 'reject' WHERE id = 'decision-1'",
		"UPDATE publication_effects SET effect_type = 'review_comment' WHERE id = 'effect-1'",
		"UPDATE publication_attempts SET outcome = 'uncertain' WHERE id = 'attempt-1'",
	} {
		if _, err := store.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("updating immutable ledger record was accepted: %s", statement)
		}
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE watch_rules
SET enabled = 0, updated_at_us = 61 WHERE id = 'rule-1'`); err != nil {
		t.Fatalf("stable watch rule mutable fields rejected: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE watch_rules
SET rule_key = 'renamed', updated_at_us = 62 WHERE id = 'rule-1'`); err == nil {
		t.Fatal("stable watch rule identity update was accepted")
	}

	var mode string
	if err := store.db.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = 'publication_mode'`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "disabled" {
		t.Fatalf("publication mode = %q, want disabled", mode)
	}
	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
	if ids.AssessmentID == "" || ids.RevisionID == "" {
		t.Fatalf("missing evidence IDs: %+v", ids)
	}
}

func TestPolicyAndPublicationLedgerRejectsStaleOrUnauthorizedEffects(t *testing.T) {
	ctx := context.Background()
	store, ids := seedPolicyPublicationChain(t, ctx)

	_, err := store.db.ExecContext(ctx, `INSERT INTO publication_effects(
 id, owner_kind, owner_id, proposal_revision_id, authorization_decision_id,
 connection_id, repository_id, pull_request_id, revision_id, observation_id,
 effect_type, payload_json, payload_sha256, idempotency_key,
 publication_mode_at_authorization, created_at_us)
VALUES ('effect-rejected', 'proposal_revision', 'proposal-revision-1',
 'proposal-revision-1', NULL, 'connection-1', 'repo-1', 'pr-1', ?, ?,
 'review_approval', '{}', ?, 'effect:rejected', 'disabled', 70)`,
		ids.RevisionID, ids.ObservationID, policyDigestFor)
	if err == nil {
		t.Fatalf("unauthorized proposal effect accepted: %v", err)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state
SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.ExecContext(ctx, `INSERT INTO publication_effects(
 id, owner_kind, owner_id, proposal_revision_id, authorization_decision_id,
 connection_id, repository_id, pull_request_id, revision_id, observation_id,
 effect_type, payload_json, payload_sha256, idempotency_key,
 publication_mode_at_authorization, created_at_us)
VALUES ('effect-stale', 'operational_lifecycle', 'marker:1', NULL, NULL,
 'connection-1', 'repo-1', 'pr-1', ?, ?, 'marker_create', '{}', ?,
 'effect:stale', 'disabled', 71)`, ids.RevisionID, ids.ObservationID, policyDigestFor)
	if err == nil || !strings.Contains(err.Error(), "current canonical") {
		t.Fatalf("stale publication effect accepted: %v", err)
	}

	_, err = store.db.ExecContext(ctx, `INSERT INTO policy_evaluations(
 id, assessment_id, run_id, intent_id, connection_id, repository_id,
 pull_request_id, revision_id, observation_id, policy_set_id,
 matched_rule_id, matched_rule_version_id, profile_id, profile_version_id,
 disposition, input_json, input_sha256, safety_overrides_json,
 rendered_output_sha256, created_at_us)
VALUES ('policy-evaluation-stale', ?, ?, ?, 'connection-1', 'repo-1',
 'pr-1', ?, ?, 'policy-set-1', 'rule-1', 'rule-version-1',
 'profile-1', 'profile-version-1', 'propose_approval', '{}', ?, '[]', ?, 72)`,
		ids.AssessmentID, ids.RunID, ids.IntentID, ids.RevisionID, ids.ObservationID,
		policyDigestFor, policyDigestTwo)
	if err == nil || !strings.Contains(err.Error(), "current canonical") {
		t.Fatalf("stale policy evaluation accepted: %v", err)
	}
}

type policyPublicationIDs struct {
	IntentID      string
	RunID         string
	AssessmentID  string
	RevisionID    string
	ObservationID string
}

func seedPolicyPublicationChain(t *testing.T, ctx context.Context) (*Store, policyPublicationIDs) {
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
	target, err := store.LoadCurrentCanonicalReviewTarget(ctx, "connection-1", "pr-1")
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
			t.Fatalf("seed policy: %v\n%s", err, statement)
		}
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO policy_evaluations(
 id, assessment_id, run_id, intent_id, connection_id, repository_id,
 pull_request_id, revision_id, observation_id, policy_set_id,
 matched_rule_id, matched_rule_version_id, profile_id, profile_version_id,
 disposition, input_json, input_sha256, safety_overrides_json,
 rendered_output_sha256, created_at_us)
VALUES ('policy-evaluation-1', ?, ?, ?, 'connection-1', 'repo-1',
 'pr-1', ?, ?, 'policy-set-1', 'rule-1', 'rule-version-1',
 'profile-1', 'profile-version-1', 'propose_approval', '{}', ?, '[]', ?, 52)`,
		recorded.AssessmentID, prepared.RunID, prepared.IntentID, target.RevisionID,
		target.ObservationID, policyDigestTri, policyDigestFor); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO proposals(
 id, policy_evaluation_id, assessment_id, run_id, intent_id,
 pull_request_id, revision_id, observation_id, proposal_kind, created_at_us)
VALUES ('proposal-1', 'policy-evaluation-1', ?, ?, ?, 'pr-1', ?, ?, 'approval', 53)`,
		recorded.AssessmentID, prepared.RunID, prepared.IntentID, target.RevisionID, target.ObservationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO proposal_revisions(
 id, proposal_id, policy_evaluation_id, assessment_id, run_id, intent_id,
 pull_request_id, revision_id, observation_id, revision_number, editor_kind,
 body, inline_comments_json, content_sha256, created_at_us)
VALUES ('proposal-revision-1', 'proposal-1', 'policy-evaluation-1', ?, ?, ?,
 'pr-1', ?, ?, 1, 'policy', '', '[]', ?, 54)`,
		recorded.AssessmentID, prepared.RunID, prepared.IntentID, target.RevisionID,
		target.ObservationID, policyDigestOne); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO decisions(
 id, proposal_id, proposal_revision_id, policy_evaluation_id, assessment_id,
 run_id, intent_id, connection_id, repository_id, pull_request_id, revision_id,
 observation_id, decision, actor_kind, actor_id, idempotency_key, reason, created_at_us)
VALUES ('decision-1', 'proposal-1', 'proposal-revision-1', 'policy-evaluation-1',
 ?, ?, ?, 'connection-1', 'repo-1', 'pr-1', ?, ?, 'approve', 'human',
 'local-user', 'decision:1', NULL, 55)`, recorded.AssessmentID, prepared.RunID,
		prepared.IntentID, target.RevisionID, target.ObservationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO publication_effects(
 id, owner_kind, owner_id, proposal_revision_id, authorization_decision_id,
 connection_id, repository_id, pull_request_id, revision_id, observation_id,
 effect_type, payload_json, payload_sha256, idempotency_key,
 publication_mode_at_authorization, created_at_us)
VALUES ('effect-1', 'proposal_revision', 'proposal-revision-1',
 'proposal-revision-1', 'decision-1', 'connection-1', 'repo-1', 'pr-1', ?, ?,
 'review_approval', '{}', ?, 'effect:1', 'disabled', 56)`,
		target.RevisionID, target.ObservationID, policyDigestTwo); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO publication_attempts(
 id, effect_id, attempt_number, publication_mode, outcome, request_sha256,
 response_json, error_class, error_message, github_artifact_id,
 attempted_at_us, completed_at_us, created_at_us)
VALUES ('attempt-1', 'effect-1', 1, 'simulated', 'simulated', ?, '{}',
 NULL, NULL, NULL, 57, 57, 57)`, policyDigestTri); err != nil {
		t.Fatal(err)
	}
	return store, policyPublicationIDs{
		IntentID: prepared.IntentID, RunID: prepared.RunID, AssessmentID: recorded.AssessmentID,
		RevisionID: target.RevisionID, ObservationID: target.ObservationID,
	}
}
