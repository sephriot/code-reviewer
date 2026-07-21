package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	githubadapter "github.com/sephriot/code-reviewer/internal/adapters/github"
)

func TestReconcilePaginatesDeduplicatesAndProjectsBothScopes(t *testing.T) {
	reader := &fakeReader{
		user: testAuthenticatedUser(),
		search: map[string]map[int]fakeSearchResult{
			"is:pr state:open review-requested:reviewer": {
				1: {page: githubadapter.SearchPage{TotalCount: 1, NextPage: 2, Candidates: []githubadapter.SearchCandidate{{Owner: "acme", Repository: "widgets", Number: 42}}}},
				2: {page: githubadapter.SearchPage{TotalCount: 1, Candidates: []githubadapter.SearchCandidate{{Owner: "ACME", Repository: "widgets", Number: 42}}}},
			},
			"is:pr state:open author:reviewer": {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
		},
		details: map[string]fakeDetailResult{"acme/widgets#42": {result: githubadapter.PullRequestResult{PullRequest: testPullRequest(true, false)}}},
	}
	store := newFakeStore()
	service, _ := NewService(reader, store)
	report, err := service.Reconcile(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Scopes) != 2 || report.Scopes[0].State != GenerationComplete || report.Scopes[0].PagesReceived != 2 || report.Scopes[0].Candidates != 1 {
		t.Fatalf("report = %+v", report)
	}
	assigned := store.applied[0]
	if len(assigned.Items) != 1 || assigned.Items[0].RelationshipKind != RelationshipReviewRequested || len(assigned.Closures) != 0 {
		t.Fatalf("assigned apply = %+v", assigned)
	}
	if assigned.Items[0].PullRequest.FactsSHA256 == "" || assigned.Items[0].PullRequest.BodySHA256 == "" {
		t.Fatalf("projection lacks hashes: %+v", assigned.Items[0])
	}
	if store.applied[1].State != GenerationComplete || len(store.applied[1].Items) != 0 {
		t.Fatalf("authored apply = %+v", store.applied[1])
	}
}

func TestPartialSearchNeverClosesAbsentRelationship(t *testing.T) {
	reader := &fakeReader{
		user: testAuthenticatedUser(),
		search: map[string]map[int]fakeSearchResult{
			"is:pr state:open review-requested:reviewer": {1: {page: githubadapter.SearchPage{TotalCount: 0, IncompleteResults: true}}},
			"is:pr state:open author:reviewer":           {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
		}, details: map[string]fakeDetailResult{},
	}
	store := newFakeStore()
	store.active[ScopeReviewRequested] = []ActiveRelationship{testActiveRelationship()}
	service, _ := NewService(reader, store)
	if _, err := service.Reconcile(context.Background(), testConfig()); err != nil {
		t.Fatal(err)
	}
	if store.applied[0].State != GenerationPartial || len(store.applied[0].Closures) != 0 || reader.detailCalls != 0 {
		t.Fatalf("partial apply = %+v, detail calls=%d", store.applied[0], reader.detailCalls)
	}
}

func TestCompleteAbsenceClosesOnlyAfterDirectRefreshProof(t *testing.T) {
	reader := &fakeReader{
		user: testAuthenticatedUser(),
		search: map[string]map[int]fakeSearchResult{
			"is:pr state:open review-requested:reviewer": {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
			"is:pr state:open author:reviewer":           {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
		},
		details: map[string]fakeDetailResult{"acme/widgets#42": {result: githubadapter.PullRequestResult{PullRequest: testPullRequest(false, false)}}},
	}
	store := newFakeStore()
	store.active[ScopeReviewRequested] = []ActiveRelationship{testActiveRelationship()}
	service, _ := NewService(reader, store)
	if _, err := service.Reconcile(context.Background(), testConfig()); err != nil {
		t.Fatal(err)
	}
	apply := store.applied[0]
	if apply.State != GenerationComplete || len(apply.Closures) != 1 || apply.Closures[0].RelationshipID != "relationship-1" {
		t.Fatalf("apply = %+v", apply)
	}
	if len(apply.Items) != 1 || apply.Items[0].RelationshipKind != "" {
		t.Fatalf("closure evidence item = %+v", apply.Items)
	}
}

func TestSearchIndexGapRetainsRelationshipAndMarksPartial(t *testing.T) {
	reader := &fakeReader{
		user: testAuthenticatedUser(),
		search: map[string]map[int]fakeSearchResult{
			"is:pr state:open review-requested:reviewer": {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
			"is:pr state:open author:reviewer":           {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
		},
		details: map[string]fakeDetailResult{"acme/widgets#42": {result: githubadapter.PullRequestResult{PullRequest: testPullRequest(true, false)}}},
	}
	store := newFakeStore()
	store.active[ScopeReviewRequested] = []ActiveRelationship{testActiveRelationship()}
	service, _ := NewService(reader, store)
	report, err := service.Reconcile(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if report.Scopes[0].State != GenerationPartial || report.Scopes[0].SearchGaps != 1 || len(store.applied[0].Closures) != 0 {
		t.Fatalf("report=%+v apply=%+v", report.Scopes[0], store.applied[0])
	}
}

func TestSearchFailureIsPersistedWithoutStoppingOtherScope(t *testing.T) {
	reader := &fakeReader{
		user: testAuthenticatedUser(),
		search: map[string]map[int]fakeSearchResult{
			"is:pr state:open review-requested:reviewer": {1: {err: errors.New("network unavailable")}},
			"is:pr state:open author:reviewer":           {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
		}, details: map[string]fakeDetailResult{},
	}
	store := newFakeStore()
	service, _ := NewService(reader, store)
	report, err := service.Reconcile(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if report.Scopes[0].State != GenerationFailed || report.Scopes[1].State != GenerationComplete || len(store.applied) != 2 {
		t.Fatalf("report=%+v applied=%+v", report, store.applied)
	}
	if strings.Contains(store.applied[0].ErrorMessage, "\n") {
		t.Fatalf("unsafe error message = %q", store.applied[0].ErrorMessage)
	}
}

func TestSearchCoverageMismatchIsPartial(t *testing.T) {
	for _, test := range []struct {
		name  string
		pages map[int]fakeSearchResult
		class string
	}{
		{
			name: "missing next link",
			pages: map[int]fakeSearchResult{1: {page: githubadapter.SearchPage{
				TotalCount: 2, Candidates: []githubadapter.SearchCandidate{{Owner: "acme", Repository: "widgets", Number: 42}},
			}}},
			class: "provider_coverage_mismatch",
		},
		{
			name: "total changes between pages",
			pages: map[int]fakeSearchResult{
				1: {page: githubadapter.SearchPage{TotalCount: 2, NextPage: 2, Candidates: []githubadapter.SearchCandidate{{Owner: "acme", Repository: "widgets", Number: 42}}}},
				2: {page: githubadapter.SearchPage{TotalCount: 1, Candidates: []githubadapter.SearchCandidate{{Owner: "acme", Repository: "other", Number: 7}}}},
			},
			class: "provider_total_changed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &fakeReader{
				user: testAuthenticatedUser(),
				search: map[string]map[int]fakeSearchResult{
					"is:pr state:open review-requested:reviewer": test.pages,
					"is:pr state:open author:reviewer":           {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
				},
				details: map[string]fakeDetailResult{
					"acme/widgets#42": {result: githubadapter.PullRequestResult{PullRequest: testPullRequest(true, false)}},
					"acme/other#7":    {result: githubadapter.PullRequestResult{PullRequest: testPullRequestFor("acme/other", 7, true)}},
				},
			}
			store := newFakeStore()
			service, _ := NewService(reader, store)
			if _, err := service.Reconcile(context.Background(), testConfig()); err != nil {
				t.Fatal(err)
			}
			if store.applied[0].State != GenerationPartial || store.applied[0].ErrorClass != test.class || len(store.applied[0].Closures) != 0 {
				t.Fatalf("apply = %+v", store.applied[0])
			}
		})
	}
}

func TestStaleDirectRefreshRetainsRelationship(t *testing.T) {
	stale := testPullRequest(false, false)
	stale.UpdatedAt = time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	reader := &fakeReader{
		user: testAuthenticatedUser(),
		search: map[string]map[int]fakeSearchResult{
			"is:pr state:open review-requested:reviewer": {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
			"is:pr state:open author:reviewer":           {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
		},
		details: map[string]fakeDetailResult{"acme/widgets#42": {result: githubadapter.PullRequestResult{PullRequest: stale}}},
	}
	store := newFakeStore()
	active := testActiveRelationship()
	active.CurrentGitHubUpdatedAt = time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	store.active[ScopeReviewRequested] = []ActiveRelationship{active}
	service, _ := NewService(reader, store)
	report, err := service.Reconcile(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if report.Scopes[0].State != GenerationPartial || report.Scopes[0].StaleDetails != 1 || len(store.applied[0].Closures) != 0 {
		t.Fatalf("report=%+v apply=%+v", report.Scopes[0], store.applied[0])
	}
}

type fakeSearchResult struct {
	page githubadapter.SearchPage
	err  error
}

type fakeDetailResult struct {
	result githubadapter.PullRequestResult
	err    error
}

type fakeReader struct {
	user        githubadapter.AuthenticatedUserResult
	search      map[string]map[int]fakeSearchResult
	details     map[string]fakeDetailResult
	detailCalls int
}

func (f *fakeReader) AuthenticatedUser(context.Context) (githubadapter.AuthenticatedUserResult, error) {
	return f.user, nil
}

func (f *fakeReader) SearchPullRequests(_ context.Context, query string, page int) (githubadapter.SearchPage, error) {
	result, exists := f.search[query][page]
	if !exists {
		return githubadapter.SearchPage{}, errors.New("unexpected search")
	}
	return result.page, result.err
}

func (f *fakeReader) GetPullRequest(_ context.Context, owner, repository string, number int, _ string) (githubadapter.PullRequestResult, error) {
	f.detailCalls++
	result, exists := f.details[candidateKey(owner, repository, number)]
	if !exists {
		return githubadapter.PullRequestResult{}, errors.New("unexpected detail")
	}
	return result.result, result.err
}

type fakeStore struct {
	next    int64
	active  map[ScopeKind][]ActiveRelationship
	applied []ApplyGenerationInput
}

func newFakeStore() *fakeStore { return &fakeStore{active: make(map[ScopeKind][]ActiveRelationship)} }

func (f *fakeStore) UpsertGitHubConnection(context.Context, ConnectionInput) error { return nil }

func (f *fakeStore) NextReconciliationGeneration(_ context.Context, scope Scope, _ time.Time) (Generation, error) {
	f.next++
	return Generation{ID: "generation-" + string(rune('0'+f.next)), Number: f.next, Scope: scope}, nil
}

func (f *fakeStore) ListActiveRelationships(_ context.Context, scope Scope) ([]ActiveRelationship, error) {
	return f.active[scope.Kind], nil
}

func (f *fakeStore) ApplyReconciliationGeneration(_ context.Context, input ApplyGenerationInput) (ApplyGenerationResult, error) {
	f.applied = append(f.applied, input)
	return ApplyGenerationResult{NewObservations: len(input.Items), ClosedRelationships: len(input.Closures)}, nil
}

func testAuthenticatedUser() githubadapter.AuthenticatedUserResult {
	return githubadapter.AuthenticatedUserResult{User: githubadapter.User{ID: 9001, NodeID: "U_9001", Login: "reviewer"}}
}

func testConfig() Config {
	return Config{ConnectionID: "connection-1", APIBaseURL: "https://api.github.com", CredentialRefKind: "environment", CredentialLocator: "env:GITHUB_TOKEN"}
}

func testPullRequest(requested, authored bool) *githubadapter.PullRequest {
	author := githubadapter.User{ID: 8001, NodeID: "U_8001", Login: "author"}
	if authored {
		author = testAuthenticatedUser().User
	}
	reviewers := []githubadapter.User{}
	if requested {
		reviewers = append(reviewers, testAuthenticatedUser().User)
	}
	return &githubadapter.PullRequest{
		ID: 501, NodeID: "PR_501", Number: 42, URL: "https://github.com/acme/widgets/pull/42",
		Title: "Projection", Body: "body", Author: author,
		TargetRepository: githubadapter.Repository{ID: 77, NodeID: "R_77", FullName: "acme/widgets"},
		State:            "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
		BaseRef: "main", Labels: []string{"bug"}, RequestedReviewers: reviewers,
		UpdatedAt: time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC),
	}
}

func testPullRequestFor(fullName string, number int, requested bool) *githubadapter.PullRequest {
	pullRequest := testPullRequest(requested, false)
	pullRequest.ID += int64(number)
	pullRequest.NodeID += string(rune('A' + number))
	pullRequest.Number = number
	pullRequest.TargetRepository.ID += int64(number)
	pullRequest.TargetRepository.NodeID += string(rune('A' + number))
	pullRequest.TargetRepository.FullName = fullName
	return pullRequest
}

func testActiveRelationship() ActiveRelationship {
	return ActiveRelationship{
		ID: "relationship-1", Kind: RelationshipReviewRequested,
		SubjectLogin: "reviewer", SubjectDatabaseID: 9001,
		RepositoryOwner: "acme", RepositoryName: "widgets", PullRequestNumber: 42,
		CurrentGitHubUpdatedAt: time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC),
	}
}
