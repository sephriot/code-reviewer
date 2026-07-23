package sqlite

import (
	"context"
	"reflect"
	"strings"
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

func TestListCurrentAttentionExcludesTerminalPullRequests(t *testing.T) {
	ctx := context.Background()
	store, target := seedCurrentCanonicalReviewTarget(t, ctx)
	seedReviewProfileVersion(t, ctx, store, "profile-1", "profile-version-1")
	var nextObservationAt, nextProjectionAt int64
	if err := store.db.QueryRowContext(ctx, `
SELECT observation.github_updated_at_us + 1, projection.updated_at_us + 1
FROM pull_request_projection_state AS projection
JOIN pull_request_observations AS observation ON observation.id = projection.current_observation_id
WHERE projection.pull_request_id = 'pr-1'`).Scan(&nextObservationAt, &nextProjectionAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_observations(
 id, connection_id, repository_id, pull_request_id, revision_id, head_sha, base_sha,
 source_kind, source_priority, facts_format_version, facts_sha256, title,
 author_login, author_database_id, body_sha256, labels_json, is_draft, base_ref,
 requested_reviewers_json, relationship_set_json, github_state,
 github_updated_at_us, observed_at_us, created_at_us)
VALUES ('merged-observation', 'connection-1', 'repo-1', 'pr-1', ?, ?, ?,
 'direct_refresh', 30, 1, ?, 'Merged pull request', 'author', 8001, ?, '[]', 0, 'main', '[]', '[]', 'merged',
 ?, ?, ?)`, target.RevisionID, projectionHeadSHA, projectionBaseSHA, strings.Repeat("f", 64), projectionDigest, nextObservationAt, nextObservationAt, nextObservationAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
UPDATE pull_request_projection_state
SET current_observation_id = 'merged-observation', updated_at_us = ?
WHERE pull_request_id = 'pr-1'`, nextProjectionAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO review_intents(
 id, connection_id, repository_id, pull_request_id, revision_id, observation_id,
 profile_id, profile_version_id, trigger_kind, idempotency_key, trigger_sha256,
 requested_at_us, created_at_us)
VALUES ('merged-pr-intent', 'connection-1', 'repo-1', 'pr-1', ?, 'merged-observation',
 'profile-1', 'profile-version-1', 'manual', 'merged-pr-run', ?, 110, 110)`,
		target.RevisionID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO review_runs(
 id, intent_id, connection_id, pull_request_id, revision_id, observation_id,
 attempt_number, engine_kind, engine_config_json, started_at_us, created_at_us)
VALUES ('merged-pr-run', 'merged-pr-intent', 'connection-1', 'pr-1', ?, 'merged-observation',
 1, 'cli', '{}', 110, 110)`, target.RevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO review_run_events(id, run_id, sequence, event_kind, payload_json, occurred_at_us, created_at_us)
VALUES ('merged-pr-run-failed', 'merged-pr-run', 1, 'failed_terminal', '{"code":"engine_protocol_invalid"}', 110, 110)`); err != nil {
		t.Fatal(err)
	}

	page, err := store.ListCurrentAttention(ctx, AttentionQuery{ConnectionID: "connection-1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("terminal pull request remained in attention: %+v", page.Items)
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
