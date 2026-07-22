package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPublicationDispatchLoadsCurrentEffectAndRecordsOneAttempt(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeSimulated)
	effect := createPublicationEffect(t, ctx, store, fixture.proposalRevisionID, "publish:dispatch:one")

	target, err := store.LoadCurrentPublicationEffect(ctx, effect.EffectID)
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != effect.EffectID || target.PublicationMode != PublicationModeSimulated {
		t.Fatalf("target = %+v", target)
	}

	first, err := store.RecordSimulatedPublicationAttempt(ctx, effect.EffectID, time.Unix(81, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RecordSimulatedPublicationAttempt(ctx, effect.EffectID, time.Unix(82, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.AttemptID == "" || second.Created || second.AttemptID != first.AttemptID {
		t.Fatalf("attempts = %+v / %+v", first, second)
	}
	assertTableCount(t, ctx, store.db, "publication_attempts", 1)

	var mode, outcome, response string
	if err := store.db.QueryRowContext(ctx, `SELECT publication_mode, outcome, response_json
FROM publication_attempts WHERE id = ?`, first.AttemptID).Scan(&mode, &outcome, &response); err != nil {
		t.Fatal(err)
	}
	if mode != "simulated" || outcome != "simulated" || response != `{"simulated":true}` {
		t.Fatalf("attempt = mode=%q outcome=%q response=%q", mode, outcome, response)
	}
}

func TestPublicationDispatchDisabledEffectNeverCreatesAttempt(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	effect := createPublicationEffect(t, ctx, store, fixture.proposalRevisionID, "publish:dispatch:disabled")

	target, err := store.LoadCurrentPublicationEffect(ctx, effect.EffectID)
	if err != nil {
		t.Fatal(err)
	}
	if target.PublicationMode != PublicationModeDisabled {
		t.Fatalf("target = %+v", target)
	}
	attempt, err := store.RecordSimulatedPublicationAttempt(ctx, effect.EffectID, time.Unix(83, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Created || attempt.AttemptID != "" {
		t.Fatalf("attempt = %+v", attempt)
	}
	assertTableCount(t, ctx, store.db, "publication_attempts", 0)
}

func TestClaimEnabledPublicationAttemptPersistsOnePreSendClaim(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeEnabled)
	effect := createPublicationEffect(t, ctx, store, fixture.proposalRevisionID, "publish:claim:enabled")

	first, err := store.ClaimEnabledPublicationAttempt(ctx, effect.EffectID, time.Unix(91, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.ClaimID == "" || first.Effect.ID != effect.EffectID ||
		first.Effect.PublicationMode != PublicationModeEnabled || first.Effect.Owner == "" ||
		first.Effect.Repository == "" || first.Effect.PullRequestNumber < 1 ||
		first.Effect.EffectType != "review_changes" || len(first.Effect.PayloadJSON) == 0 ||
		first.Effect.PayloadSHA256 == "" || !first.ClaimedAt.Equal(time.Unix(91, 0).UTC()) {
		t.Fatalf("first claim = %+v", first)
	}
	second, err := store.ClaimEnabledPublicationAttempt(ctx, effect.EffectID, time.Unix(92, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.ClaimID != first.ClaimID || !second.ClaimedAt.Equal(first.ClaimedAt) {
		t.Fatalf("second claim = %+v", second)
	}
	assertTableCount(t, ctx, store.db, "publication_dispatch_claims", 1)
	assertTableCount(t, ctx, store.db, "publication_attempts", 0)
}

func TestClaimEnabledPublicationAttemptRejectsNonEnabledAndStaleEffects(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	effect := createPublicationEffect(t, ctx, store, fixture.proposalRevisionID, "publish:claim:disabled")
	if _, err := store.ClaimEnabledPublicationAttempt(ctx, effect.EffectID, time.Unix(93, 0).UTC()); !errors.Is(err, ErrPublicationEffectNotDispatchable) {
		t.Fatalf("disabled claim = %v", err)
	}
	assertTableCount(t, ctx, store.db, "publication_dispatch_claims", 0)

	store, fixture = seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeEnabled)
	effect = createPublicationEffect(t, ctx, store, fixture.proposalRevisionID, "publish:claim:stale")
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state
SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimEnabledPublicationAttempt(ctx, effect.EffectID, time.Unix(94, 0).UTC()); !errors.Is(err, ErrPublicationEffectNotCurrent) {
		t.Fatalf("stale claim = %v", err)
	}
	assertTableCount(t, ctx, store.db, "publication_dispatch_claims", 0)
}

func TestPublicationDispatchRejectsAbsentStaleAndConflictingEffects(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeSimulated)
	effect := createPublicationEffect(t, ctx, store, fixture.proposalRevisionID, "publish:dispatch:conflict")

	if _, err := store.LoadCurrentPublicationEffect(ctx, "missing-effect"); !errors.Is(err, ErrPublicationEffectNotFound) {
		t.Fatalf("missing effect = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state
SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadCurrentPublicationEffect(ctx, effect.EffectID); !errors.Is(err, ErrPublicationEffectNotCurrent) {
		t.Fatalf("stale effect = %v", err)
	}
	if _, err := store.RecordSimulatedPublicationAttempt(ctx, effect.EffectID, time.Unix(84, 0).UTC()); !errors.Is(err, ErrPublicationEffectNotCurrent) {
		t.Fatalf("stale record = %v", err)
	}

	store, fixture = seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeSimulated)
	effect = createPublicationEffect(t, ctx, store, fixture.proposalRevisionID, "publish:dispatch:bad-attempt")
	if _, err := store.db.ExecContext(ctx, `INSERT INTO publication_attempts(
 id, effect_id, attempt_number, publication_mode, outcome, request_sha256,
 response_json, attempted_at_us, completed_at_us, created_at_us)
SELECT 'bad-attempt', id, 1, 'simulated', 'simulated', payload_sha256,
       '{"simulated":false}', 85, 85, 85
FROM publication_effects WHERE id = ?`, effect.EffectID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordSimulatedPublicationAttempt(ctx, effect.EffectID, time.Unix(86, 0).UTC()); !errors.Is(err, ErrPublicationAttemptConflict) {
		t.Fatalf("conflicting attempt = %v", err)
	}
}

func createPublicationEffect(t *testing.T, ctx context.Context, store *Store, proposalRevisionID, idempotencyKey string) CreatePublicationEffectResult {
	t.Helper()
	result, err := store.CreatePublicationEffect(ctx, CreatePublicationEffectInput{
		ProposalRevisionID: proposalRevisionID,
		IdempotencyKey:     idempotencyKey,
		CreatedAt:          time.Unix(80, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func setPublicationMode(t *testing.T, ctx context.Context, store *Store, mode PublicationMode) {
	t.Helper()
	if _, err := store.db.ExecContext(ctx, `UPDATE system_state SET value = ?, updated_at_us = 79
WHERE key = 'publication_mode'`, mode); err != nil {
		t.Fatal(err)
	}
}
