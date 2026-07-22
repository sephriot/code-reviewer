package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCreatePublicationEffectDisabledPersistsAuthorizedEffectOnly(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)

	result, err := store.CreatePublicationEffect(ctx, CreatePublicationEffectInput{
		ProposalRevisionID: fixture.proposalRevisionID,
		IdempotencyKey:     "publish:disabled:1",
		CreatedAt:          time.Unix(70, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.EffectID == "" || result.PublicationMode != PublicationModeDisabled {
		t.Fatalf("result = %+v", result)
	}
	assertTableCount(t, ctx, store.db, "publication_effects", 1)
	assertTableCount(t, ctx, store.db, "publication_attempts", 0)
	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}

	var effectType, payload, payloadSHA, decisionID, mode string
	if err := store.db.QueryRowContext(ctx, `
SELECT effect_type, payload_json, payload_sha256, authorization_decision_id,
       publication_mode_at_authorization
FROM publication_effects WHERE id = ?`, result.EffectID).Scan(
		&effectType, &payload, &payloadSHA, &decisionID, &mode); err != nil {
		t.Fatal(err)
	}
	if effectType != "review_changes" || payload != `{"body":"Request nil guard.\n","inline_comments":[{"path":"internal/example.go"}]}` ||
		payloadSHA == "" || decisionID != fixture.decisionID || mode != "disabled" {
		t.Fatalf("effect = type=%q payload=%q sha=%q decision=%q mode=%q", effectType, payload, payloadSHA, decisionID, mode)
	}
}

func TestCreatePublicationEffectSimulatedPersistsEffectWithoutAttempt(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	if _, err := store.db.ExecContext(ctx, `UPDATE system_state
SET value = 'simulated', updated_at_us = 61 WHERE key = 'publication_mode'`); err != nil {
		t.Fatal(err)
	}

	result, err := store.CreatePublicationEffect(ctx, CreatePublicationEffectInput{
		ProposalRevisionID: fixture.proposalRevisionID,
		IdempotencyKey:     "publish:simulated:1",
		CreatedAt:          time.Unix(70, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.EffectID == "" || result.PublicationMode != PublicationModeSimulated {
		t.Fatalf("result = %+v", result)
	}
	assertTableCount(t, ctx, store.db, "publication_effects", 1)
	assertTableCount(t, ctx, store.db, "publication_attempts", 0)
}

func TestCreatePublicationEffectIdempotenceConflictAndSafety(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	input := CreatePublicationEffectInput{
		ProposalRevisionID: fixture.proposalRevisionID,
		IdempotencyKey:     "publish:stable:1",
		CreatedAt:          time.Unix(70, 0).UTC(),
	}
	first, err := store.CreatePublicationEffect(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreatePublicationEffect(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || first.EffectID != second.EffectID {
		t.Fatalf("results = %+v / %+v", first, second)
	}
	_, err = store.CreatePublicationEffect(ctx, CreatePublicationEffectInput{
		ProposalRevisionID: fixture.proposalRevisionID,
		IdempotencyKey:     "publish:duplicate-intent:1",
		CreatedAt:          input.CreatedAt,
	})
	if !errors.Is(err, ErrPublicationEffectConflict) {
		t.Fatalf("semantic conflict = %v", err)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE system_state
SET value = 'simulated', updated_at_us = 70 WHERE key = 'publication_mode'`); err != nil {
		t.Fatal(err)
	}
	_, err = store.CreatePublicationEffect(ctx, CreatePublicationEffectInput{
		ProposalRevisionID: fixture.proposalRevisionID,
		IdempotencyKey:     input.IdempotencyKey,
		CreatedAt:          input.CreatedAt,
	})
	if !errors.Is(err, ErrPublicationEffectConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	assertTableCount(t, ctx, store.db, "publication_effects", 1)

	if _, err := store.db.ExecContext(ctx, `UPDATE system_state
SET value = 'enabled', updated_at_us = 71 WHERE key = 'publication_mode'`); err != nil {
		t.Fatal(err)
	}
	_, err = store.CreatePublicationEffect(ctx, CreatePublicationEffectInput{
		ProposalRevisionID: fixture.proposalRevisionID,
		IdempotencyKey:     "publish:enabled:1",
		CreatedAt:          time.Unix(71, 0).UTC(),
	})
	if !errors.Is(err, ErrPublicationEffectConflict) {
		t.Fatalf("enabled mode = %v", err)
	}
	assertTableCount(t, ctx, store.db, "publication_effects", 1)

	if _, err := store.db.ExecContext(ctx, `UPDATE system_state
SET value = 'disabled', updated_at_us = 72 WHERE key = 'publication_mode'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state
SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	_, err = store.CreatePublicationEffect(ctx, CreatePublicationEffectInput{
		ProposalRevisionID: fixture.proposalRevisionID,
		IdempotencyKey:     "publish:stale:1",
		CreatedAt:          time.Unix(72, 0).UTC(),
	})
	if !errors.Is(err, ErrCanonicalReviewTargetNotFound) || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("stale evidence = %v", err)
	}
}

func TestCreatePublicationEffectEnabledPersistsIntentWithoutAttempt(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeEnabled)

	result, err := store.CreatePublicationEffect(ctx, CreatePublicationEffectInput{
		ProposalRevisionID: fixture.proposalRevisionID,
		IdempotencyKey:     "publish:enabled:1",
		CreatedAt:          time.Unix(71, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.PublicationMode != PublicationModeEnabled {
		t.Fatalf("result = %+v", result)
	}
	assertTableCount(t, ctx, store.db, "publication_effects", 1)
	assertTableCount(t, ctx, store.db, "publication_attempts", 0)
	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func TestCreatePublicationEffectAndEnsureJobIsAtomic(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeSimulated)
	_, err := store.CreatePublicationEffectAndEnsureJob(ctx, CreatePublicationEffectInput{ProposalRevisionID: fixture.proposalRevisionID, IdempotencyKey: "publish:atomic", CreatedAt: time.Unix(80, 0).UTC()}, func(string, PublicationMode) (JobInput, error) { return JobInput{}, errors.New("stop") })
	if err == nil {
		t.Fatal("job factory failure accepted")
	}
	assertTableCount(t, ctx, store.db, "publication_effects", 0)
	assertTableCount(t, ctx, store.db, "jobs", 0)
}

func TestCreatePublicationEffectRejectsDisallowedModeWithoutEffect(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeEnabled)

	_, err := store.CreatePublicationEffect(ctx, CreatePublicationEffectInput{
		ProposalRevisionID: fixture.proposalRevisionID,
		AllowedModes:       []PublicationMode{PublicationModeSimulated},
		CreatedAt:          time.Unix(72, 0).UTC(),
	})
	if !errors.Is(err, ErrPublicationModeNotAllowed) {
		t.Fatalf("CreatePublicationEffect() error = %v", err)
	}
	for _, table := range []string{"publication_effects", "publication_attempts", "jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

type approvedPublicationProposalFixture struct {
	proposalRevisionID string
	decisionID         string
}

func seedApprovedPublicationProposal(t *testing.T, ctx context.Context) (*Store, approvedPublicationProposalFixture) {
	t.Helper()
	store, policyFixture := seedPolicyRecordFixture(t, ctx)
	proposal, err := store.RecordPolicyEvaluation(ctx, policyRecordInput(policyFixture))
	if err != nil {
		t.Fatal(err)
	}
	var runID, intentID, revisionID, observationID string
	if err := store.db.QueryRowContext(ctx, `
SELECT run_id, intent_id, revision_id, observation_id
FROM policy_evaluations WHERE id = ?`, proposal.PolicyEvaluationID).Scan(&runID, &intentID, &revisionID, &observationID); err != nil {
		t.Fatal(err)
	}
	const decisionID = "publication-decision-1"
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO decisions(
 id, proposal_id, proposal_revision_id, policy_evaluation_id, assessment_id,
 run_id, intent_id, connection_id, repository_id, pull_request_id, revision_id,
 observation_id, decision, actor_kind, actor_id, idempotency_key, reason, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, 'connection-1', 'repo-1', 'pr-1', ?, ?, 'approve',
 'human', 'local-user', 'decision:publication:1', NULL, 65)`,
		decisionID, proposal.ProposalID, proposal.ProposalRevisionID, proposal.PolicyEvaluationID,
		policyFixture.assessmentID, runID, intentID, revisionID, observationID); err != nil {
		t.Fatal(err)
	}
	return store, approvedPublicationProposalFixture{
		proposalRevisionID: proposal.ProposalRevisionID, decisionID: decisionID,
	}
}
