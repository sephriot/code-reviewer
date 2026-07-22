package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCreateHumanProposalRevisionAppendsNormalizedCurrentEvidence(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)
	proposal, err := store.RecordPolicyEvaluation(ctx, policyRecordInput(fixture))
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.CreateHumanProposalRevision(ctx, CreateHumanProposalRevisionInput{
		ProposalID:         proposal.ProposalID,
		Body:               "Human edit.\r\n",
		InlineCommentsJSON: []byte(` [ { "path": "internal/example.go" } ] `),
		EditedAt:           time.Unix(70, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ProposalRevisionID == "" || created.RevisionNumber != 2 {
		t.Fatalf("created = %+v", created)
	}
	var editor, body, comments string
	if err := store.db.QueryRowContext(ctx, `
SELECT editor_kind, body, inline_comments_json
FROM proposal_revisions WHERE id = ?`, created.ProposalRevisionID).Scan(&editor, &body, &comments); err != nil {
		t.Fatal(err)
	}
	if editor != "human" || body != "Human edit.\n" || comments != `[{"path":"internal/example.go"}]` {
		t.Fatalf("stored revision = %q %q %q", editor, body, comments)
	}
	for _, table := range []string{"decisions", "publication_effects", "publication_attempts", "jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func TestCreateHumanProposalRevisionRequiresCurrentEvidenceAndAppends(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)
	proposal, err := store.RecordPolicyEvaluation(ctx, policyRecordInput(fixture))
	if err != nil {
		t.Fatal(err)
	}
	input := CreateHumanProposalRevisionInput{ProposalID: proposal.ProposalID, Body: "first", EditedAt: time.Unix(70, 0).UTC()}
	first, err := store.CreateHumanProposalRevision(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input.Body = "second"
	second, err := store.CreateHumanProposalRevision(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.RevisionNumber != 2 || second.RevisionNumber != 3 || first.ProposalRevisionID == second.ProposalRevisionID {
		t.Fatalf("revisions = %+v / %+v", first, second)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state
SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateHumanProposalRevision(ctx, input)
	if !errors.Is(err, ErrCanonicalReviewTargetNotFound) {
		t.Fatalf("stale edit error = %v", err)
	}
	assertTableCount(t, ctx, store.db, "proposal_revisions", 3)
}

func TestCreateHumanProposalRevisionRejectsInvalidContent(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)
	proposal, err := store.RecordPolicyEvaluation(ctx, policyRecordInput(fixture))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateHumanProposalRevision(ctx, CreateHumanProposalRevisionInput{
		ProposalID: proposal.ProposalID, InlineCommentsJSON: []byte(`{}`), EditedAt: time.Unix(70, 0).UTC(),
	})
	if err == nil || !strings.Contains(err.Error(), "inline comments") {
		t.Fatalf("invalid comments error = %v", err)
	}
	assertTableCount(t, ctx, store.db, "proposal_revisions", 1)
}

func TestRecordProposalDecisionUsesStableIdempotencyAndCurrentEvidence(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)
	proposal, err := store.RecordPolicyEvaluation(ctx, policyRecordInput(fixture))
	if err != nil {
		t.Fatal(err)
	}
	input := RecordProposalDecisionInput{
		ProposalRevisionID: proposal.ProposalRevisionID,
		Decision:           ProposalDecisionApprove,
		ActorKind:          ProposalDecisionActorHuman,
		ActorID:            "local-user",
		IdempotencyKey:     "human:proposal-1:approve",
		Reason:             "Looks good.\r\n",
		DecidedAt:          time.Unix(80, 0).UTC(),
	}
	first, err := store.RecordProposalDecision(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input.DecidedAt = input.DecidedAt.Add(time.Hour)
	second, err := store.RecordProposalDecision(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || first.DecisionID != second.DecisionID {
		t.Fatalf("decisions = %+v / %+v", first, second)
	}
	var decision, actor, reason string
	if err := store.db.QueryRowContext(ctx, `SELECT decision, actor_kind, reason FROM decisions WHERE id = ?`, first.DecisionID).Scan(&decision, &actor, &reason); err != nil {
		t.Fatal(err)
	}
	if decision != "approve" || actor != "human" || reason != "Looks good.\n" {
		t.Fatalf("stored decision = %q %q %q", decision, actor, reason)
	}
	for _, table := range []string{"publication_effects", "publication_attempts", "jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func TestRecordProposalDecisionForProposalRequiresRouteProposalOwnership(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)
	proposal, err := store.RecordPolicyEvaluation(ctx, policyRecordInput(fixture))
	if err != nil {
		t.Fatal(err)
	}
	input := RecordProposalDecisionInput{
		ProposalRevisionID: proposal.ProposalRevisionID,
		Decision:           ProposalDecisionApprove,
		ActorKind:          ProposalDecisionActorHuman,
		ActorID:            "local-user",
		IdempotencyKey:     "human:proposal-1:approve",
		DecidedAt:          time.Unix(80, 0).UTC(),
	}
	if _, err := store.RecordProposalDecisionForProposal(ctx, "other-proposal", input); !errors.Is(err, ErrProposalNotFound) {
		t.Fatalf("wrong proposal error = %v", err)
	}
	result, err := store.RecordProposalDecisionForProposal(ctx, proposal.ProposalID, input)
	if err != nil || !result.Created {
		t.Fatalf("owned proposal decision = %+v, %v", result, err)
	}
	assertTableCount(t, ctx, store.db, "decisions", 1)
}

func TestRecordProposalDecisionRejectsConflictingOrStaleFacts(t *testing.T) {
	ctx := context.Background()
	store, fixture := seedPolicyRecordFixture(t, ctx)
	proposal, err := store.RecordPolicyEvaluation(ctx, policyRecordInput(fixture))
	if err != nil {
		t.Fatal(err)
	}
	input := RecordProposalDecisionInput{
		ProposalRevisionID: proposal.ProposalRevisionID,
		Decision:           ProposalDecisionReject, ActorKind: ProposalDecisionActorPolicy, ActorID: "policy-v1",
		IdempotencyKey: "policy:proposal-1:reject", DecidedAt: time.Unix(80, 0).UTC(),
	}
	if _, err := store.RecordProposalDecision(ctx, input); err != nil {
		t.Fatal(err)
	}
	input.Decision = ProposalDecisionApprove
	if _, err := store.RecordProposalDecision(ctx, input); !errors.Is(err, ErrProposalDecisionConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	input.Decision = ProposalDecisionReject
	input.IdempotencyKey = "different-request"
	if _, err := store.RecordProposalDecision(ctx, input); !errors.Is(err, ErrProposalDecisionConflict) {
		t.Fatalf("revision conflict = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state
SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	input.ProposalRevisionID = proposal.ProposalRevisionID
	input.IdempotencyKey = "after-stale"
	if _, err := store.RecordProposalDecision(ctx, input); !errors.Is(err, ErrCanonicalReviewTargetNotFound) {
		t.Fatalf("stale decision error = %v", err)
	}
	assertTableCount(t, ctx, store.db, "decisions", 1)
}
