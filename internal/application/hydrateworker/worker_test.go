package hydrateworker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/application/hydrate"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

func TestSchedulerEnqueuesOneJobPerCurrentObservation(t *testing.T) {
	t.Parallel()
	store := &schedulerStore{targets: []sqlite.CanonicalHydrationTarget{
		testTarget("observation-1", 7), testTarget("observation-2", 8),
	}}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	results, err := (Scheduler{Store: store, Now: func() time.Time { return now }}).Schedule(context.Background(), "connection-1")
	if err != nil {
		t.Fatal(err)
	}
	if store.connectionID != "connection-1" || len(results) != 2 || len(store.inputs) != 2 {
		t.Fatalf("connection=%q results=%+v inputs=%+v", store.connectionID, results, store.inputs)
	}
	for index, input := range store.inputs {
		target := store.targets[index]
		if input.Kind != HydrateJobKind || input.ResourceType != "pull_request_observation" || input.ResourceID != target.ObservationID || input.DedupeKey != HydrateJobKind+":"+target.ObservationID {
			t.Fatalf("job input = %+v", input)
		}
		if !input.AvailableAt.Equal(now) || input.MaxAttempts != 3 {
			t.Fatalf("job timing = %+v", input)
		}
		payload, err := parseJobPayload(input.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if payload.ObservationID != target.ObservationID || payload.Number != target.Number || payload.ConnectionID != target.ConnectionID {
			t.Fatalf("payload = %+v target = %+v", payload, target)
		}
		if strings.Contains(string(input.Payload), "secret") || strings.Contains(string(input.Payload), "token") {
			t.Fatalf("payload carries secret material: %s", input.Payload)
		}
	}
}

func TestSchedulerRejectsInvalidTargetBeforeEnqueue(t *testing.T) {
	t.Parallel()
	store := &schedulerStore{targets: []sqlite.CanonicalHydrationTarget{{ConnectionID: "connection-1", ObservationID: "", Owner: "owner", Repository: "repo", Number: 7}}}
	_, err := (Scheduler{Store: store}).Schedule(context.Background(), "connection-1")
	if err == nil || !strings.Contains(err.Error(), "hydration target") {
		t.Fatalf("error = %v", err)
	}
	if len(store.inputs) != 0 {
		t.Fatalf("invalid target enqueued: %+v", store.inputs)
	}
}

func TestTargetSchedulerEnqueuesOnlySelectedPullRequest(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	store := &targetSchedulerStore{target: testTarget("observation-7", 7)}
	result, err := (TargetScheduler{Store: store, Now: func() time.Time { return now }}).SchedulePullRequest(context.Background(), "connection-1", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if store.connectionID != "connection-1" || store.pullRequestID != "pr-1" || len(store.inputs) != 1 || result.ID != "job-1" {
		t.Fatalf("store=%+v result=%+v", store, result)
	}
	input := store.inputs[0]
	if input.ResourceID != "observation-7" || !input.AvailableAt.Equal(now) || input.DedupeKey != HydrateJobKind+":observation-7" {
		t.Fatalf("input=%+v", input)
	}
}

func TestHandlerInvokesHydrationWithPinnedObservation(t *testing.T) {
	t.Parallel()
	target := testTarget("observation-1", 7)
	store := &hydrationStore{target: target}
	var factoryConnection string
	reader := &failingReader{}
	handler := Handler{
		Store: store,
		NewReader: func(_ context.Context, connectionID string) (hydrate.Reader, error) {
			factoryConnection = connectionID
			return reader, nil
		},
	}
	payload, err := marshalJobPayload(target)
	if err != nil {
		t.Fatal(err)
	}
	err = handler.Handle(context.Background(), sqlite.Job{Kind: HydrateJobKind, Payload: payload})
	if err == nil || !strings.Contains(err.Error(), "read pull request revision") {
		t.Fatalf("error = %v", err)
	}
	if factoryConnection != "connection-1" || store.findCalls != 2 || reader.pullCalls != 1 {
		t.Fatalf("factory=%q find=%d reads=%d", factoryConnection, store.findCalls, reader.pullCalls)
	}
}

func TestHandlerRejectsSupersededObservationBeforeReader(t *testing.T) {
	t.Parallel()
	target := testTarget("observation-new", 7)
	payload, err := marshalJobPayload(testTarget("observation-old", 7))
	if err != nil {
		t.Fatal(err)
	}
	handler := Handler{
		Store: &hydrationStore{target: target},
		NewReader: func(context.Context, string) (hydrate.Reader, error) {
			return nil, errors.New("reader must not run")
		},
	}
	err = handler.Handle(context.Background(), sqlite.Job{Kind: HydrateJobKind, Payload: payload})
	if err == nil || !worker.IsPermanent(err) || !strings.Contains(err.Error(), "no longer current") {
		t.Fatalf("error = %v", err)
	}
}

func TestHandlerMarksMalformedPayloadPermanent(t *testing.T) {
	t.Parallel()
	handler := Handler{
		Store: &hydrationStore{},
		NewReader: func(context.Context, string) (hydrate.Reader, error) {
			return nil, errors.New("factory must not run")
		},
	}
	err := handler.Handle(context.Background(), sqlite.Job{Kind: HydrateJobKind, Payload: []byte(`{"version":1,"connection_id":"connection-1","observation_id":"observation-1","owner":"owner","repository":"repo","number":7,"token":"secret"}`)})
	if err == nil || !worker.IsPermanent(err) || !strings.Contains(err.Error(), "malformed hydration job payload") {
		t.Fatalf("error = %v", err)
	}
}

type schedulerStore struct {
	targets      []sqlite.CanonicalHydrationTarget
	connectionID string
	inputs       []sqlite.JobInput
}

type targetSchedulerStore struct {
	target        sqlite.CanonicalHydrationTarget
	connectionID  string
	pullRequestID string
	inputs        []sqlite.JobInput
}

func (s *targetSchedulerStore) FindCanonicalHydrationTargetByPullRequestID(_ context.Context, connectionID, pullRequestID string) (sqlite.CanonicalHydrationTarget, error) {
	s.connectionID, s.pullRequestID = connectionID, pullRequestID
	return s.target, nil
}

func (s *targetSchedulerStore) EnsureJob(_ context.Context, input sqlite.JobInput) (sqlite.EnsureJobResult, error) {
	s.inputs = append(s.inputs, input)
	return sqlite.EnsureJobResult{ID: "job-1", Created: true}, nil
}

func (s *schedulerStore) ListCanonicalHydrationTargets(_ context.Context, connectionID string) ([]sqlite.CanonicalHydrationTarget, error) {
	s.connectionID = connectionID
	return s.targets, nil
}

func (s *schedulerStore) EnsureJob(_ context.Context, input sqlite.JobInput) (sqlite.EnsureJobResult, error) {
	s.inputs = append(s.inputs, input)
	return sqlite.EnsureJobResult{ID: "job-" + input.ResourceID, Created: true}, nil
}

type hydrationStore struct {
	target    sqlite.CanonicalHydrationTarget
	findCalls int
}

func (s *hydrationStore) FindCanonicalHydrationTarget(_ context.Context, connectionID, owner, repository string, number int) (sqlite.CanonicalHydrationTarget, error) {
	s.findCalls++
	return s.target, nil
}

func (*hydrationStore) AttachCanonicalRevision(context.Context, sqlite.CanonicalRevisionInput) (sqlite.CanonicalRevisionResult, error) {
	return sqlite.CanonicalRevisionResult{}, errors.New("attach must not run")
}

type failingReader struct{ pullCalls int }

func (r *failingReader) GetPullRequest(context.Context, string, string, int, string) (github.PullRequestResult, error) {
	r.pullCalls++
	return github.PullRequestResult{}, errors.New("stop")
}

func (*failingReader) GetPullRequestDiff(context.Context, string, string, int, string) (github.PullRequestDiffResult, error) {
	return github.PullRequestDiffResult{}, errors.New("unexpected diff")
}

func (*failingReader) GetPullRequestFiles(context.Context, string, string, int, int) (github.PullRequestFilesPage, error) {
	return github.PullRequestFilesPage{}, errors.New("unexpected files")
}

func (*failingReader) GetGitTree(context.Context, string, string, string) (github.GitTreeResult, error) {
	return github.GitTreeResult{}, errors.New("unexpected tree")
}

func testTarget(observationID string, number int) sqlite.CanonicalHydrationTarget {
	return sqlite.CanonicalHydrationTarget{
		ConnectionID: "connection-1", ObservationID: observationID, PullRequestID: "pr-1", RepositoryID: "repo-1",
		Owner: "owner", Repository: "repo", Number: number,
		HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
	}
}
