package reconcileworker_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	githubadapter "github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/application/reconcile"
	"github.com/sephriot/code-reviewer/internal/application/reconcileworker"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

func TestSchedulerEnsuresOneRedactedConnectionJob(t *testing.T) {
	t.Parallel()
	store := &ensureJobStore{}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	scheduler := reconcileworker.Scheduler{Store: store, Now: func() time.Time { return now }}

	result, err := scheduler.Schedule(context.Background(), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.ID != "job-1" || len(store.inputs) != 1 {
		t.Fatalf("result=%+v inputs=%+v", result, store.inputs)
	}
	input := store.inputs[0]
	if input.Kind != reconcileworker.ReconcileJobKind || input.ResourceType != "github_connection" || input.ResourceID != "connection-1" || input.DedupeKey != "github.reconcile.v1:connection-1" {
		t.Fatalf("job input = %+v", input)
	}
	if !input.AvailableAt.Equal(now) || input.MaxAttempts != 3 {
		t.Fatalf("schedule facts = %+v", input)
	}
	if strings.Contains(string(input.Payload), "secret") || strings.Contains(string(input.Payload), "token") {
		t.Fatalf("payload carries secret material: %s", input.Payload)
	}
	var payload map[string]any
	if err := json.Unmarshal(input.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if got, want := payload, map[string]any{
		"version": float64(1), "connection_id": "connection-1", "api_base_url": "https://api.github.com",
		"credential_ref_kind": "environment", "credential_locator": "env:GITHUB_TOKEN",
	}; !mapsEqual(got, want) {
		t.Fatalf("payload = %#v, want %#v", got, want)
	}
}

func TestSchedulerRejectsMalformedConfigBeforeEnqueue(t *testing.T) {
	t.Parallel()
	store := &ensureJobStore{}
	_, err := (reconcileworker.Scheduler{Store: store}).Schedule(context.Background(), reconcile.Config{ConnectionID: "connection-1"})
	if err == nil || !strings.Contains(err.Error(), "connection ID") {
		t.Fatalf("error = %v", err)
	}
	if len(store.inputs) != 0 {
		t.Fatalf("enqueued malformed config: %+v", store.inputs)
	}
}

func TestHandlerReconcilesUsingInjectedReader(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{
		user: testAuthenticatedUser(),
		search: map[string]map[int]fakeSearchResult{
			"is:pr state:open review-requested:reviewer": {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
			"is:pr state:open author:reviewer":           {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
		},
	}
	var factoryConfig reconcile.Config
	handler := reconcileworker.Handler{
		Store: newFakeStore(),
		NewReader: func(_ context.Context, config reconcile.Config) (githubadapter.Reader, error) {
			factoryConfig = config
			return reader, nil
		},
	}
	if err := handler.Handle(context.Background(), sqlite.Job{Kind: reconcileworker.ReconcileJobKind, Payload: mustJobPayload(testConfig())}); err != nil {
		t.Fatal(err)
	}
	if factoryConfig != testConfig() {
		t.Fatalf("factory config = %+v", factoryConfig)
	}
}

func TestHandlerSchedulesHydrationOnlyAfterSuccessfulReconciliation(t *testing.T) {
	reader := &fakeReader{user: testAuthenticatedUser(), search: map[string]map[int]fakeSearchResult{
		"is:pr state:open review-requested:reviewer": {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
		"is:pr state:open author:reviewer":           {1: {page: githubadapter.SearchPage{TotalCount: 0}}},
	}}
	scheduler := &hydrationSchedulerRecorder{}
	handler := reconcileworker.Handler{Store: newFakeStore(), NewReader: func(context.Context, reconcile.Config) (githubadapter.Reader, error) { return reader, nil }, HydrationScheduler: scheduler}
	if err := handler.Handle(context.Background(), sqlite.Job{Kind: reconcileworker.ReconcileJobKind, Payload: mustJobPayload(testConfig())}); err != nil {
		t.Fatal(err)
	}
	if scheduler.connectionID != "connection-1" {
		t.Fatalf("hydration connection=%q", scheduler.connectionID)
	}

	scheduler.err = errors.New("job database offline")
	err := handler.Handle(context.Background(), sqlite.Job{Kind: reconcileworker.ReconcileJobKind, Payload: mustJobPayload(testConfig())})
	if err == nil || worker.IsPermanent(err) || !strings.Contains(err.Error(), "schedule canonical hydration") {
		t.Fatalf("schedule error=%v", err)
	}
}

func TestHandlerMarksMalformedPayloadPermanent(t *testing.T) {
	t.Parallel()
	handler := reconcileworker.Handler{
		Store: newFakeStore(),
		NewReader: func(context.Context, reconcile.Config) (githubadapter.Reader, error) {
			return nil, errors.New("factory must not run")
		},
	}
	err := handler.Handle(context.Background(), sqlite.Job{
		Kind:    reconcileworker.ReconcileJobKind,
		Payload: []byte(`{"version":1,"connection_id":"connection-1","api_base_url":"https://api.github.com","credential_ref_kind":"environment","credential_locator":"env:GITHUB_TOKEN","token":"secret"}`),
	})
	if err == nil || !worker.IsPermanent(err) || !strings.Contains(err.Error(), "malformed reconciliation job payload") {
		t.Fatalf("error = %v", err)
	}
}

type ensureJobStore struct{ inputs []sqlite.JobInput }

type hydrationSchedulerRecorder struct {
	connectionID string
	err          error
}

func (s *hydrationSchedulerRecorder) Schedule(_ context.Context, connectionID string) ([]sqlite.EnsureJobResult, error) {
	s.connectionID = connectionID
	return nil, s.err
}

func (s *ensureJobStore) EnsureJob(_ context.Context, input sqlite.JobInput) (sqlite.EnsureJobResult, error) {
	s.inputs = append(s.inputs, input)
	return sqlite.EnsureJobResult{ID: "job-1", Created: true}, nil
}

func mapsEqual(got, want map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			return false
		}
	}
	return true
}

func testConfig() reconcile.Config {
	return reconcile.Config{ConnectionID: "connection-1", APIBaseURL: "https://api.github.com", CredentialRefKind: "environment", CredentialLocator: "env:GITHUB_TOKEN"}
}

func mustJobPayload(config reconcile.Config) []byte {
	return []byte(`{"version":1,"connection_id":"` + config.ConnectionID + `","api_base_url":"` + config.APIBaseURL + `","credential_ref_kind":"` + config.CredentialRefKind + `","credential_locator":"` + config.CredentialLocator + `"}`)
}

type fakeSearchResult struct {
	page githubadapter.SearchPage
	err  error
}

type fakeReader struct {
	user   githubadapter.AuthenticatedUserResult
	search map[string]map[int]fakeSearchResult
}

func (f *fakeReader) AuthenticatedUser(context.Context) (githubadapter.AuthenticatedUserResult, error) {
	return f.user, nil
}

func (f *fakeReader) SearchPullRequests(_ context.Context, query string, page int) (githubadapter.SearchPage, error) {
	result, found := f.search[query][page]
	if !found {
		return githubadapter.SearchPage{}, errors.New("unexpected search")
	}
	return result.page, result.err
}

func (f *fakeReader) GetPullRequest(context.Context, string, string, int, string) (githubadapter.PullRequestResult, error) {
	return githubadapter.PullRequestResult{}, errors.New("unexpected detail")
}

func testAuthenticatedUser() githubadapter.AuthenticatedUserResult {
	return githubadapter.AuthenticatedUserResult{User: githubadapter.User{ID: 9001, NodeID: "U_9001", Login: "reviewer"}}
}

type fakeStore struct{}

func newFakeStore() *fakeStore { return &fakeStore{} }

func (*fakeStore) UpsertGitHubConnection(context.Context, reconcile.ConnectionInput) error {
	return nil
}

func (*fakeStore) NextReconciliationGeneration(_ context.Context, scope reconcile.Scope, _ time.Time) (reconcile.Generation, error) {
	return reconcile.Generation{ID: "generation-1", Number: 1, Scope: scope}, nil
}

func (*fakeStore) ListActiveRelationships(context.Context, reconcile.Scope) ([]reconcile.ActiveRelationship, error) {
	return nil, nil
}

func (*fakeStore) ApplyReconciliationGeneration(context.Context, reconcile.ApplyGenerationInput) (reconcile.ApplyGenerationResult, error) {
	return reconcile.ApplyGenerationResult{}, nil
}
