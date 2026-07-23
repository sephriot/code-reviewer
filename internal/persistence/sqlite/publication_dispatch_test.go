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

func TestPublicationEffectStatusReturnsSafeAttemptState(t *testing.T) {
	ctx := context.Background()
	store, _ := seedPolicyPublicationChain(t, ctx)

	status, err := store.PublicationEffectStatus(ctx, "effect-1")
	if err != nil {
		t.Fatal(err)
	}
	if status.EffectID != "effect-1" || status.PublicationMode != PublicationModeDisabled || status.Attempt == nil ||
		status.Attempt.AttemptID != "attempt-1" || status.Attempt.PublicationMode != PublicationModeSimulated ||
		status.Attempt.Outcome != "simulated" || !status.Attempt.CompletedAt.Equal(time.UnixMicro(57).UTC()) ||
		status.Resolution != nil {
		t.Fatalf("status = %+v", status)
	}
	if _, err := store.PublicationEffectStatus(ctx, "missing"); !errors.Is(err, ErrPublicationEffectNotFound) {
		t.Fatalf("missing status error = %v", err)
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

func TestRecordEnabledPublicationAttemptRequiresClaimAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeEnabled)
	effect := createPublicationEffect(t, ctx, store, fixture.proposalRevisionID, "publish:outcome:enabled")
	input := RecordEnabledPublicationAttemptInput{
		EffectID: effect.EffectID, Outcome: PublicationAttemptSucceeded,
		ResponseJSON: []byte(`{"state":"APPROVED"}`), GitHubArtifactID: "77",
		CompletedAt: time.Unix(101, 0).UTC(),
	}
	if _, err := store.RecordEnabledPublicationAttempt(ctx, input); !errors.Is(err, ErrPublicationEffectNotDispatchable) {
		t.Fatalf("unclaimed outcome = %v", err)
	}
	if _, err := store.ClaimEnabledPublicationAttempt(ctx, effect.EffectID, time.Unix(100, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	first, err := store.RecordEnabledPublicationAttempt(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.RecordEnabledPublicationAttempt(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.AttemptID == "" || second.Created || second.AttemptID != first.AttemptID {
		t.Fatalf("attempts = %+v / %+v", first, second)
	}
	assertTableCount(t, ctx, store.db, "publication_attempts", 1)

	conflict := input
	conflict.ResponseJSON = []byte(`{"state":"COMMENTED"}`)
	if _, err := store.RecordEnabledPublicationAttempt(ctx, conflict); !errors.Is(err, ErrPublicationAttemptConflict) {
		t.Fatalf("conflicting outcome = %v", err)
	}
}

func TestRecordEnabledPublicationAttemptRejectsUnsafeOutcomeMetadata(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeEnabled)
	effect := createPublicationEffect(t, ctx, store, fixture.proposalRevisionID, "publish:outcome:invalid")
	if _, err := store.ClaimEnabledPublicationAttempt(ctx, effect.EffectID, time.Unix(102, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	for _, input := range []RecordEnabledPublicationAttemptInput{
		{EffectID: effect.EffectID, Outcome: PublicationAttemptSucceeded, ResponseJSON: []byte(`[]`), GitHubArtifactID: "77"},
		{EffectID: effect.EffectID, Outcome: PublicationAttemptSucceeded, ResponseJSON: []byte(`{}`)},
		{EffectID: effect.EffectID, Outcome: PublicationAttemptUncertain, ResponseJSON: []byte(`{}`)},
		{EffectID: effect.EffectID, Outcome: "failed_retryable", ResponseJSON: []byte(`{}`), ErrorClass: "network", ErrorMessage: "lost"},
	} {
		if _, err := store.RecordEnabledPublicationAttempt(ctx, input); err == nil {
			t.Fatalf("accepted unsafe input = %+v", input)
		}
	}
	assertTableCount(t, ctx, store.db, "publication_attempts", 0)
}

func TestResolveUncertainPublicationIsImmutableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeEnabled)
	effect := createPublicationEffect(t, ctx, store, fixture.proposalRevisionID, "publish:resolve:uncertain")
	if _, err := store.ClaimEnabledPublicationAttempt(ctx, effect.EffectID, time.Unix(103, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.RecordEnabledPublicationAttempt(ctx, RecordEnabledPublicationAttemptInput{
		EffectID: effect.EffectID, Outcome: PublicationAttemptUncertain, ResponseJSON: []byte(`{}`),
		ErrorClass: "connection_reset", ErrorMessage: "connection closed after write", CompletedAt: time.Unix(104, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	input := ResolvePublicationUncertaintyInput{
		EffectID: effect.EffectID, Resolution: PublicationUncertaintyExternallyCompleted,
		ActorID: "operator-1", IdempotencyKey: "resolve:one", Reason: "Verified review exists in GitHub.", ResolvedAt: time.Unix(105, 0).UTC(),
	}
	first, err := store.ResolvePublicationUncertainty(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ResolvePublicationUncertainty(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.ResolutionID == "" || second.Created || second.ResolutionID != first.ResolutionID {
		t.Fatalf("results = %+v / %+v", first, second)
	}
	assertTableCount(t, ctx, store.db, "publication_uncertainty_resolutions", 1)
	var storedAttemptID, resolution, actor, key, reason string
	if err := store.db.QueryRowContext(ctx, `SELECT attempt_id, resolution, actor_id, idempotency_key, reason
FROM publication_uncertainty_resolutions WHERE id = ?`, first.ResolutionID).Scan(&storedAttemptID, &resolution, &actor, &key, &reason); err != nil {
		t.Fatal(err)
	}
	if storedAttemptID != attempt.AttemptID || resolution != string(PublicationUncertaintyExternallyCompleted) || actor != input.ActorID || key != input.IdempotencyKey || reason != input.Reason {
		t.Fatalf("stored resolution = attempt=%q resolution=%q actor=%q key=%q reason=%q", storedAttemptID, resolution, actor, key, reason)
	}
	history, err := store.ListHistory(ctx, HistoryQuery{ConnectionID: "connection-1", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPublicationResolutionHistory(history.Items, first.ResolutionID) {
		t.Fatalf("history missing resolution: %+v", history.Items)
	}
	timeline, err := store.PullRequestTimeline(ctx, PullRequestTimelineQuery{ConnectionID: "connection-1", PullRequestID: "pr-1", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPublicationResolutionTimeline(timeline.Items, first.ResolutionID) {
		t.Fatalf("timeline missing resolution: %+v", timeline.Items)
	}
	conflict := input
	conflict.Resolution = PublicationUncertaintyAbandoned
	if _, err := store.ResolvePublicationUncertainty(ctx, conflict); !errors.Is(err, ErrPublicationUncertaintyResolutionConflict) {
		t.Fatalf("conflicting resolution = %v", err)
	}
}

func containsPublicationResolutionHistory(items []HistoryItem, resolutionID string) bool {
	for _, item := range items {
		if item.Kind == HistoryKindPublicationResolution && item.ID == resolutionID {
			return true
		}
	}
	return false
}

func containsPublicationResolutionTimeline(items []TimelineItem, resolutionID string) bool {
	for _, item := range items {
		if item.Kind == TimelineKindPublicationResolution && item.ID == resolutionID {
			return true
		}
	}
	return false
}

func TestResolveUncertainPublicationRejectsWithoutUncertainty(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedApprovedPublicationProposal(t, ctx)
	setPublicationMode(t, ctx, store, PublicationModeEnabled)
	effect := createPublicationEffect(t, ctx, store, fixture.proposalRevisionID, "publish:resolve:succeeded")
	if _, err := store.ClaimEnabledPublicationAttempt(ctx, effect.EffectID, time.Unix(106, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordEnabledPublicationAttempt(ctx, RecordEnabledPublicationAttemptInput{
		EffectID: effect.EffectID, Outcome: PublicationAttemptSucceeded, ResponseJSON: []byte(`{"state":"APPROVED"}`), GitHubArtifactID: "88", CompletedAt: time.Unix(107, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := store.ResolvePublicationUncertainty(ctx, ResolvePublicationUncertaintyInput{
		EffectID: effect.EffectID, Resolution: PublicationUncertaintyAbandoned, ActorID: "operator-1", IdempotencyKey: "resolve:no", ResolvedAt: time.Unix(108, 0).UTC(),
	})
	if !errors.Is(err, ErrPublicationUncertaintyNotResolvable) {
		t.Fatalf("resolution = %v", err)
	}
	assertTableCount(t, ctx, store.db, "publication_uncertainty_resolutions", 0)
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
