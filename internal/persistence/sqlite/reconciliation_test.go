package sqlite

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/reconcile"
)

func TestApplyReconciliationGenerationBridgesLegacyIdentityWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	now := time.Unix(100, 0).UTC()
	connection := testConnection(now)
	if err := store.UpsertGitHubConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO repositories(id, full_name, owner_login, name, created_at_us, updated_at_us)
VALUES ('legacy-repo', 'Owner/Repo', 'Owner', 'Repo', 1, 1);
INSERT INTO pull_requests(id, repository_id, number, state, created_at_us, updated_at_us)
VALUES ('legacy-pr', 'legacy-repo', 42, 'unknown', 1, 1)`); err != nil {
		t.Fatal(err)
	}

	scope := testScope(connection.ID)
	generation, err := store.NextReconciliationGeneration(ctx, scope, now)
	if err != nil {
		t.Fatal(err)
	}
	input := completeGeneration(generation, now.Add(time.Second), []reconcile.ProjectionItem{testProjectionItem()})
	result, err := store.ApplyReconciliationGeneration(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.NewRepositories != 0 || result.NewPullRequests != 0 || result.NewObservations != 1 || result.OpenedRelationships != 1 {
		t.Fatalf("result = %+v", result)
	}

	var repositoryID, pullRequestID string
	var repositoryGitHubID, pullRequestGitHubID int64
	if err := store.db.QueryRowContext(ctx, `
SELECT repository.id, repository.github_id, pull_request.id, pull_request.github_id
FROM repositories repository JOIN pull_requests pull_request ON pull_request.repository_id = repository.id
WHERE repository.full_name = 'owner/repo' COLLATE NOCASE AND pull_request.number = 42`).
		Scan(&repositoryID, &repositoryGitHubID, &pullRequestID, &pullRequestGitHubID); err != nil {
		t.Fatal(err)
	}
	if repositoryID != "legacy-repo" || pullRequestID != "legacy-pr" || repositoryGitHubID != 1001 || pullRequestGitHubID != 2001 {
		t.Fatalf("bridge = %s/%d %s/%d", repositoryID, repositoryGitHubID, pullRequestID, pullRequestGitHubID)
	}
	assertTableCount(t, ctx, store.db, "revisions", 0)
	assertTableCount(t, ctx, store.db, "jobs", 0)
	assertTableCount(t, ctx, store.db, "domain_events", 0)
	assertTableCount(t, ctx, store.db, "outbox", 0)
}

func TestConnectionRefreshAllowsLoginRenameWithStableIdentity(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	now := time.Unix(150, 0).UTC()
	connection := testConnection(now)
	if err := store.UpsertGitHubConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	connection.AccountLogin = "renamed-reviewer"
	connection.CheckedAt = now.Add(time.Second)
	if err := store.UpsertGitHubConnection(ctx, connection); err != nil {
		t.Fatalf("refresh after login rename: %v", err)
	}
	var login string
	if err := store.db.QueryRowContext(ctx, `SELECT account_login FROM connections WHERE id = ?`, connection.ID).Scan(&login); err != nil {
		t.Fatal(err)
	}
	if login != connection.AccountLogin {
		t.Fatalf("login = %q, want %q", login, connection.AccountLogin)
	}
	connection.AccountNodeID = "different-node"
	connection.CheckedAt = now.Add(2 * time.Second)
	if err := store.UpsertGitHubConnection(ctx, connection); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("node identity change error = %v", err)
	}
}

func TestProjectionFailsBeforeWritesWhenPublicationEnabled(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	now := time.Unix(175, 0).UTC()
	connection := testConnection(now)
	if err := store.UpsertGitHubConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	generation, err := store.NextReconciliationGeneration(ctx, testScope(connection.ID), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE system_state SET value = 'enabled' WHERE key = 'publication_mode'`); err != nil {
		t.Fatal(err)
	}
	_, err = store.ApplyReconciliationGeneration(ctx, completeGeneration(generation, now.Add(time.Second), []reconcile.ProjectionItem{testProjectionItem()}))
	if err == nil || !strings.Contains(err.Error(), "publication mode disabled") {
		t.Fatalf("apply error = %v", err)
	}
	assertTableCount(t, ctx, store.db, "repositories", 0)
	var state string
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM reconciliation_generations WHERE id = ?`, generation.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("generation state = %q, want running", state)
	}
}

func TestReconciliationDeduplicatesObservationButRecordsEveryMembership(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	now := time.Unix(200, 0).UTC()
	connection := testConnection(now)
	if err := store.UpsertGitHubConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	scope := testScope(connection.ID)
	first, err := store.NextReconciliationGeneration(ctx, scope, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyReconciliationGeneration(ctx, completeGeneration(first, now.Add(time.Second), []reconcile.ProjectionItem{testProjectionItem()})); err != nil {
		t.Fatal(err)
	}
	second, err := store.NextReconciliationGeneration(ctx, scope, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.ApplyReconciliationGeneration(ctx, completeGeneration(second, now.Add(3*time.Second), []reconcile.ProjectionItem{testProjectionItem()}))
	if err != nil {
		t.Fatal(err)
	}
	if result.NewObservations != 0 || result.OpenedRelationships != 0 || result.ClosedRelationships != 0 {
		t.Fatalf("second result = %+v", result)
	}
	assertTableCount(t, ctx, store.db, "pull_request_observations", 1)
	assertTableCount(t, ctx, store.db, "reconciliation_generation_items", 2)
	assertTableCount(t, ctx, store.db, "pr_relationships", 1)
	var lastAttempt, lastComplete string
	if err := store.db.QueryRowContext(ctx, `SELECT last_attempt_generation_id, last_complete_generation_id FROM reconciliation_checkpoints`).Scan(&lastAttempt, &lastComplete); err != nil {
		t.Fatal(err)
	}
	if lastAttempt != second.ID || lastComplete != second.ID {
		t.Fatalf("checkpoint = %s/%s", lastAttempt, lastComplete)
	}
}

func TestPartialGenerationKeepsPositivesAndRejectsClosure(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	now := time.Unix(300, 0).UTC()
	connection := testConnection(now)
	if err := store.UpsertGitHubConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	scope := testScope(connection.ID)
	first, _ := store.NextReconciliationGeneration(ctx, scope, now)
	if _, err := store.ApplyReconciliationGeneration(ctx, completeGeneration(first, now.Add(time.Second), []reconcile.ProjectionItem{testProjectionItem()})); err != nil {
		t.Fatal(err)
	}
	active, err := store.ListActiveRelationships(ctx, scope)
	if err != nil || len(active) != 1 {
		t.Fatalf("active = %+v, err = %v", active, err)
	}
	second, _ := store.NextReconciliationGeneration(ctx, scope, now.Add(2*time.Second))
	partial := reconcile.ApplyGenerationInput{
		Generation: second, State: reconcile.GenerationPartial, PagesReceived: 1,
		ErrorClass: "detail_failed", FinishedAt: now.Add(3 * time.Second),
		Closures: []reconcile.RelationshipClosure{{RelationshipID: active[0].ID}},
	}
	if _, err := store.ApplyReconciliationGeneration(ctx, partial); err == nil || !strings.Contains(err.Error(), "closures require complete") {
		t.Fatalf("closure error = %v", err)
	}
	var state string
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM reconciliation_generations WHERE id = ?`, second.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("rolled-back generation state = %q", state)
	}

	partial.Closures = nil
	partial.Items = []reconcile.ProjectionItem{testProjectionItem()}
	if _, err := store.ApplyReconciliationGeneration(ctx, partial); err != nil {
		t.Fatal(err)
	}
	active, err = store.ListActiveRelationships(ctx, scope)
	if err != nil || len(active) != 1 {
		t.Fatalf("partial removed active relationship: %+v, %v", active, err)
	}
	var lastAttempt, lastComplete string
	if err := store.db.QueryRowContext(ctx, `SELECT last_attempt_generation_id, last_complete_generation_id FROM reconciliation_checkpoints`).Scan(&lastAttempt, &lastComplete); err != nil {
		t.Fatal(err)
	}
	if lastAttempt != second.ID || lastComplete != first.ID {
		t.Fatalf("partial checkpoint = %s/%s", lastAttempt, lastComplete)
	}
}

func TestCompleteGenerationClosesOnlyExplicitRelationship(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	now := time.Unix(400, 0).UTC()
	connection := testConnection(now)
	if err := store.UpsertGitHubConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	scope := testScope(connection.ID)
	first, _ := store.NextReconciliationGeneration(ctx, scope, now)
	if _, err := store.ApplyReconciliationGeneration(ctx, completeGeneration(first, now.Add(time.Second), []reconcile.ProjectionItem{testProjectionItem()})); err != nil {
		t.Fatal(err)
	}
	active, _ := store.ListActiveRelationships(ctx, scope)
	second, _ := store.NextReconciliationGeneration(ctx, scope, now.Add(2*time.Second))
	withoutClosure := completeGeneration(second, now.Add(3*time.Second), nil)
	if _, err := store.ApplyReconciliationGeneration(ctx, withoutClosure); err != nil {
		t.Fatal(err)
	}
	if stillActive, _ := store.ListActiveRelationships(ctx, scope); len(stillActive) != 1 {
		t.Fatal("complete search absence closed relationship")
	}
	third, _ := store.NextReconciliationGeneration(ctx, scope, now.Add(4*time.Second))
	closureEvidence := testRemovalProjectionItem()
	withClosure := completeGeneration(third, now.Add(5*time.Second), []reconcile.ProjectionItem{closureEvidence})
	providerTotal := 0
	withClosure.ProviderTotal = &providerTotal
	withClosure.Closures = []reconcile.RelationshipClosure{{RelationshipID: active[0].ID}}
	result, err := store.ApplyReconciliationGeneration(ctx, withClosure)
	if err != nil {
		t.Fatal(err)
	}
	if result.ClosedRelationships != 1 {
		t.Fatalf("closed = %d", result.ClosedRelationships)
	}
	if remaining, _ := store.ListActiveRelationships(ctx, scope); len(remaining) != 0 {
		t.Fatalf("remaining = %+v", remaining)
	}
	var endedByGeneration, sourceKind string
	if err := store.db.QueryRowContext(ctx, `
SELECT relationship.ended_by_generation_id, observation.source_kind
FROM pr_relationships relationship
JOIN pull_request_observations observation ON observation.id = relationship.ended_by_observation_id
WHERE relationship.id = ?`, active[0].ID).Scan(&endedByGeneration, &sourceKind); err != nil {
		t.Fatal(err)
	}
	if endedByGeneration != third.ID || sourceKind != string(reconcile.ObservationDirectRefresh) {
		t.Fatalf("closure provenance = %q/%q", endedByGeneration, sourceKind)
	}
}

func TestStaleDirectObservationCannotCloseCurrentRelationship(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	now := time.Unix(425, 0).UTC()
	connection := testConnection(now)
	if err := store.UpsertGitHubConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	scope := testScope(connection.ID)
	first, _ := store.NextReconciliationGeneration(ctx, scope, now)
	if _, err := store.ApplyReconciliationGeneration(ctx, completeGeneration(first, now.Add(time.Second), []reconcile.ProjectionItem{testProjectionItem()})); err != nil {
		t.Fatal(err)
	}
	active, _ := store.ListActiveRelationships(ctx, scope)
	second, _ := store.NextReconciliationGeneration(ctx, scope, now.Add(2*time.Second))
	staleEvidence := testRemovalProjectionItem()
	staleEvidence.PullRequest.GitHubUpdatedAt = time.Unix(89, 0).UTC()
	staleEvidence.PullRequest.FactsSHA256 = strings.Repeat("e", 64)
	input := completeGeneration(second, now.Add(3*time.Second), []reconcile.ProjectionItem{staleEvidence})
	providerTotal := 0
	input.ProviderTotal = &providerTotal
	input.Closures = []reconcile.RelationshipClosure{{RelationshipID: active[0].ID}}
	if _, err := store.ApplyReconciliationGeneration(ctx, input); err == nil || !strings.Contains(err.Error(), "direct observation") {
		t.Fatalf("stale closure error = %v", err)
	}
	if remaining, _ := store.ListActiveRelationships(ctx, scope); len(remaining) != 1 {
		t.Fatalf("stale closure changed active relationship: %+v", remaining)
	}
	var state string
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM reconciliation_generations WHERE id = ?`, second.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "running" {
		t.Fatalf("rolled-back generation state = %q", state)
	}
}

func TestRelationshipSurvivesAccountLoginRenameByStableDatabaseID(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	now := time.Unix(450, 0).UTC()
	connection := testConnection(now)
	if err := store.UpsertGitHubConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	oldScope := testScope(connection.ID)
	first, _ := store.NextReconciliationGeneration(ctx, oldScope, now)
	if _, err := store.ApplyReconciliationGeneration(ctx, completeGeneration(first, now.Add(time.Second), []reconcile.ProjectionItem{testProjectionItem()})); err != nil {
		t.Fatal(err)
	}
	connection.AccountLogin = "renamed-reviewer"
	connection.CheckedAt = now.Add(2 * time.Second)
	if err := store.UpsertGitHubConnection(ctx, connection); err != nil {
		t.Fatal(err)
	}
	newScope := testScope(connection.ID)
	newScope.Key = connection.AccountLogin
	second, _ := store.NextReconciliationGeneration(ctx, newScope, now.Add(3*time.Second))
	positive := testProjectionItem()
	positive.SubjectLogin = connection.AccountLogin
	result, err := store.ApplyReconciliationGeneration(ctx, completeGeneration(second, now.Add(4*time.Second), []reconcile.ProjectionItem{positive}))
	if err != nil {
		t.Fatal(err)
	}
	if result.OpenedRelationships != 1 || result.ClosedRelationships != 1 {
		t.Fatalf("rename relationship counts = %+v, want one replacement interval", result)
	}
	active, err := store.ListActiveRelationships(ctx, newScope)
	if err != nil || len(active) != 1 || active[0].SubjectLogin != connection.AccountLogin || active[0].SubjectDatabaseID != connection.AccountDatabaseID {
		t.Fatalf("renamed active relationship = %+v, err = %v", active, err)
	}
	assertTableCount(t, ctx, store.db, "pr_relationships", 2)

	third, _ := store.NextReconciliationGeneration(ctx, newScope, now.Add(5*time.Second))
	evidence := testRemovalProjectionItem()
	evidence.SubjectLogin = connection.AccountLogin
	closure := completeGeneration(third, now.Add(6*time.Second), []reconcile.ProjectionItem{evidence})
	closure.Closures = []reconcile.RelationshipClosure{{RelationshipID: active[0].ID}}
	if _, err := store.ApplyReconciliationGeneration(ctx, closure); err != nil {
		t.Fatalf("close relationship after login rename: %v", err)
	}
	if active, _ := store.ListActiveRelationships(ctx, newScope); len(active) != 0 {
		t.Fatalf("active after closure = %+v", active)
	}
}

func testConnection(now time.Time) reconcile.ConnectionInput {
	return reconcile.ConnectionInput{
		ID: "connection-test", APIBaseURL: "https://api.github.com", AccountLogin: "reviewer",
		AccountNodeID: "account-node", AccountDatabaseID: 9001,
		CredentialRefKind: "github_cli", CredentialLocator: "github-cli",
		PermissionsJSON: []byte(`{"pull_requests":"read"}`), CheckedAt: now,
	}
}

func testScope(connectionID string) reconcile.Scope {
	return reconcile.Scope{ConnectionID: connectionID, Kind: reconcile.ScopeReviewRequested, Key: "reviewer", QueryPartition: "all"}
}

func testProjectionItem() reconcile.ProjectionItem {
	return reconcile.ProjectionItem{
		Repository: reconcile.RepositoryFacts{GitHubID: 1001, NodeID: "repo-node", FullName: "owner/repo"},
		PullRequest: reconcile.PullRequestFacts{
			GitHubID: 2001, NodeID: "pr-node", Number: 42, Title: "Projection", URL: "https://github.com/owner/repo/pull/42",
			AuthorLogin: "author", AuthorDatabaseID: 8001, State: "open", HeadSHA: projectionHeadSHA, BaseSHA: projectionBaseSHA,
			BaseRef: "main", BodySHA256: projectionDigest, LabelsJSON: []byte(`["bug"]`), RequestedReviewersJSON: []byte(`["reviewer"]`),
			RelationshipSetJSON: []byte(`["review_requested"]`), FactsSHA256: projectionDigest, GitHubUpdatedAt: time.Unix(90, 0).UTC(),
		},
		RelationshipKind: reconcile.RelationshipReviewRequested, SubjectLogin: "reviewer", SubjectDatabaseID: 9001,
		ObservationSource: reconcile.ObservationReconciliation,
	}
}

func testRemovalProjectionItem() reconcile.ProjectionItem {
	item := testProjectionItem()
	item.RelationshipKind = ""
	item.ObservationSource = reconcile.ObservationDirectRefresh
	item.PullRequest.RequestedReviewersJSON = []byte(`[]`)
	item.PullRequest.RelationshipSetJSON = []byte(`[]`)
	item.PullRequest.FactsSHA256 = strings.Repeat("d", 64)
	return item
}

func completeGeneration(generation reconcile.Generation, finished time.Time, items []reconcile.ProjectionItem) reconcile.ApplyGenerationInput {
	pages := 1
	total := len(items)
	return reconcile.ApplyGenerationInput{
		Generation: generation, State: reconcile.GenerationComplete, PagesExpected: &pages, PagesReceived: pages,
		ProviderTotal: &total, FinishedAt: finished, Items: items,
	}
}
