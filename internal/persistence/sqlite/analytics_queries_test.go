package sqlite

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestAnalyticsOverviewCountsDurableReviewPolicyProposalAndPublicationFacts(t *testing.T) {
	ctx := context.Background()
	store, _ := seedPolicyPublicationChain(t, ctx)

	revision, err := store.CreateHumanProposalRevision(ctx, CreateHumanProposalRevisionInput{
		ProposalID: "proposal-1", Body: "Need changes", InlineCommentsJSON: []byte(`[]`), EditedAt: time.Unix(60, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordProposalDecision(ctx, RecordProposalDecisionInput{
		ProposalRevisionID: revision.ProposalRevisionID, Decision: ProposalDecisionReject,
		ActorKind: ProposalDecisionActorHuman, ActorID: "local-user", IdempotencyKey: "analytics:reject", DecidedAt: time.Unix(61, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO publication_attempts(
 id, effect_id, attempt_number, publication_mode, outcome, request_sha256,
 response_json, attempted_at_us, completed_at_us, created_at_us)
VALUES ('attempt-2', 'effect-1', 2, 'enabled', 'succeeded', '` + policyDigestTri + `', '{}', 62, 62, 62)`,
		`INSERT INTO publication_attempts(
 id, effect_id, attempt_number, publication_mode, outcome, request_sha256,
 response_json, error_class, error_message, attempted_at_us, completed_at_us, created_at_us)
VALUES ('attempt-3', 'effect-1', 3, 'enabled', 'failed_terminal', '` + policyDigestFor + `', '{}', 'github_http', 'denied', 63, 63, 63)`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	before := ledgerCounts(t, ctx, store)
	overview, err := store.AnalyticsOverview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := AnalyticsOverview{
		ObservedPullRequests:          1,
		ReviewRuns:                    1,
		Assessments:                   1,
		PolicyEvaluations:             1,
		HumanReviewEvaluations:        0,
		Proposals:                     1,
		ProposalRevisions:             2,
		ProposalApprovals:             1,
		ProposalRejections:            1,
		PublicationEffects:            1,
		PublicationAttempts:           3,
		SimulatedPublicationAttempts:  1,
		SuccessfulPublicationAttempts: 1,
		TerminalPublicationFailures:   1,
	}
	if overview != want {
		t.Fatalf("overview = %+v, want %+v", overview, want)
	}
	if after := ledgerCounts(t, ctx, store); !reflect.DeepEqual(after, before) {
		t.Fatalf("analytics read changed ledger: before=%v after=%v", before, after)
	}
}
