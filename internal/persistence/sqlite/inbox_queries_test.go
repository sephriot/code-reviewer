package sqlite

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestListCurrentAttentionIsBoundedCurrentAndReadOnly(t *testing.T) {
	ctx := context.Background()
	store, ids := seedPolicyPublicationChain(t, ctx)

	if _, err := store.CreateHumanProposalRevision(ctx, CreateHumanProposalRevisionInput{
		ProposalID: "proposal-1", Body: "Human edit", InlineCommentsJSON: []byte(`[]`), EditedAt: time.Unix(60, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	humanRun := prepareInboxRun(t, ctx, store, "inbox-human")
	humanAssessment, err := store.RecordAssessment(ctx, RecordAssessmentInput{
		RunID: humanRun.RunID, Result: testValidatedAssessmentResult(t), RecordedAt: time.Unix(61, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	humanEvaluation := policyRecordInput(policyRecordFixture{assessmentID: humanAssessment.AssessmentID})
	humanEvaluation.Disposition = PolicyDispositionRequireHumanReview
	humanEvaluation.RenderedBody = ""
	humanEvaluation.InlineCommentsJSON = []byte(`[]`)
	humanEvaluation.CreatedAt = time.Unix(62, 0).UTC()
	if _, err := store.RecordPolicyEvaluation(ctx, humanEvaluation); err != nil {
		t.Fatal(err)
	}
	failedRun := prepareInboxRun(t, ctx, store, "inbox-failed")
	if _, err := store.AppendReviewRunEvent(ctx, AppendReviewRunEventInput{
		RunID: failedRun.RunID, EventKind: ReviewRunEventFailedTerminal,
		DiagnosticCode: "engine_protocol_invalid", OccurredAt: time.Unix(63, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	before := ledgerCounts(t, ctx, store)
	page, err := store.ListCurrentAttention(ctx, AttentionQuery{ConnectionID: "connection-1", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor != "" {
		t.Fatalf("page = %+v", page)
	}
	for _, item := range page.Items {
		if !item.Current || item.State != TimelineStateCurrent || item.ConnectionID != "connection-1" || item.PullRequestID != "pr-1" {
			t.Fatalf("attention item is not explicit current evidence: %+v", item)
		}
	}
	if page.Items[0].Kind != AttentionKindPendingProposal {
		t.Fatalf("attention = %+v, want pending proposal", page.Items[0])
	}
	if after := ledgerCounts(t, ctx, store); !reflect.DeepEqual(after, before) {
		t.Fatalf("read-only inbox changed ledger: before=%v after=%v", before, after)
	}
	if ids.AssessmentID == "" { // Ensure the fixture's decided proposal stays out of inbox.
		t.Fatal("fixture missing assessment")
	}
}

func TestPullRequestTimelinePagesFactsAndLedgerHistoryWithoutWrites(t *testing.T) {
	ctx := context.Background()
	store, _ := seedPolicyPublicationChain(t, ctx)
	before := ledgerCounts(t, ctx, store)

	var all []TimelineItem
	cursor := ""
	for {
		page, err := store.PullRequestTimeline(ctx, PullRequestTimelineQuery{
			ConnectionID: "connection-1", PullRequestID: "pr-1", Limit: 2, Cursor: cursor,
		})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(all) < 8 {
		t.Fatalf("timeline too short: %+v", all)
	}
	wantedKinds := map[TimelineKind]bool{
		TimelineKindObservation: false, TimelineKindRevision: false, TimelineKindRun: false,
		TimelineKindAssessment: false, TimelineKindPolicyEvaluation: false, TimelineKindProposal: false,
		TimelineKindDecision: false, TimelineKindPublicationEffect: false, TimelineKindPublicationAttempt: false,
	}
	for _, item := range all {
		if item.ConnectionID != "connection-1" || item.PullRequestID != "pr-1" || item.State != TimelineStateCurrent || !item.Current {
			t.Fatalf("current timeline item = %+v", item)
		}
		if _, ok := wantedKinds[item.Kind]; ok {
			wantedKinds[item.Kind] = true
		}
	}
	for kind, found := range wantedKinds {
		if !found {
			t.Fatalf("timeline missing %q: %+v", kind, all)
		}
	}
	if after := ledgerCounts(t, ctx, store); !reflect.DeepEqual(after, before) {
		t.Fatalf("read-only timeline changed ledger: before=%v after=%v", before, after)
	}

	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	historical, err := store.PullRequestTimeline(ctx, PullRequestTimelineQuery{ConnectionID: "connection-1", PullRequestID: "pr-1", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range historical.Items {
		if item.Kind != TimelineKindObservation && (item.Current || item.State != TimelineStateHistorical) {
			t.Fatalf("historical state is not explicit: %+v", item)
		}
	}
}

func TestListHistoryPagesCompletedOutcomesWithoutWrites(t *testing.T) {
	ctx := context.Background()
	store, ids := seedPolicyPublicationChain(t, ctx)
	before := ledgerCounts(t, ctx, store)

	var all []HistoryItem
	cursor := ""
	for {
		page, err := store.ListHistory(ctx, HistoryQuery{ConnectionID: "connection-1", Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if ids.RunID == "" || len(all) != 3 {
		t.Fatalf("history = %+v", all)
	}
	wantedKinds := map[HistoryKind]bool{
		HistoryKindCompletedRun: false, HistoryKindDecision: false, HistoryKindPublicationAttempt: false,
	}
	for _, item := range all {
		if item.ConnectionID != "connection-1" || item.PullRequestID != "pr-1" || !item.Current || item.State != TimelineStateCurrent {
			t.Fatalf("history item = %+v", item)
		}
		if _, found := wantedKinds[item.Kind]; found {
			wantedKinds[item.Kind] = true
		}
	}
	for kind, found := range wantedKinds {
		if !found {
			t.Fatalf("history missing %q: %+v", kind, all)
		}
	}
	if after := ledgerCounts(t, ctx, store); !reflect.DeepEqual(after, before) {
		t.Fatalf("read-only history changed ledger: before=%v after=%v", before, after)
	}
}

func TestInboxAndTimelineRejectInvalidBoundsAndCursor(t *testing.T) {
	ctx := context.Background()
	store, _ := seedPolicyPublicationChain(t, ctx)
	for _, query := range []AttentionQuery{
		{Limit: -1}, {Limit: 101}, {ConnectionID: "connection-1", Cursor: "not-a-cursor"},
	} {
		if _, err := store.ListCurrentAttention(ctx, query); err == nil {
			t.Fatalf("invalid attention query accepted: %+v", query)
		}
	}
	for _, query := range []PullRequestTimelineQuery{
		{ConnectionID: "", PullRequestID: "pr-1"}, {ConnectionID: "connection-1", PullRequestID: "pr-1", Limit: 101},
		{ConnectionID: "connection-1", PullRequestID: "pr-1", Cursor: "not-a-cursor"},
	} {
		if _, err := store.PullRequestTimeline(ctx, query); err == nil {
			t.Fatalf("invalid timeline query accepted: %+v", query)
		}
	}
	for _, query := range []HistoryQuery{
		{Limit: -1}, {Limit: 101}, {ConnectionID: "connection-1", Cursor: "not-a-cursor"},
	} {
		if _, err := store.ListHistory(ctx, query); err == nil {
			t.Fatalf("invalid history query accepted: %+v", query)
		}
	}
}

func prepareInboxRun(t *testing.T, ctx context.Context, store *Store, idempotencyKey string) PrepareReviewRunResult {
	t.Helper()
	input := testPrepareReviewRunInput()
	input.IdempotencyKey = idempotencyKey
	result, err := store.PrepareReviewRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func ledgerCounts(t *testing.T, ctx context.Context, store *Store) map[string]int {
	t.Helper()
	tables := []string{"review_intents", "review_runs", "review_run_events", "assessments", "policy_evaluations", "proposals", "proposal_revisions", "decisions", "publication_effects", "publication_attempts", "jobs", "domain_events", "outbox"}
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[table] = count
	}
	return counts
}
