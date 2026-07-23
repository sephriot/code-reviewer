package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeHydrationScheduler struct {
	connectionID  string
	pullRequestID string
	jobID         string
	created       bool
	err           error
}

func (f *fakeHydrationScheduler) SchedulePullRequest(_ context.Context, connectionID, pullRequestID string) (HydrationScheduleResult, error) {
	f.connectionID, f.pullRequestID = connectionID, pullRequestID
	return HydrationScheduleResult{JobID: f.jobID, Created: f.created}, f.err
}

func TestHydrationMutationSchedulesSelectedPullRequest(t *testing.T) {
	t.Parallel()
	scheduler := &fakeHydrationScheduler{jobID: "job-1", created: true}
	handler := NewHydrationMutationHandler(HydrationMutationOptions{Scheduler: scheduler})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/pull-requests/pr-1/hydrate", strings.NewReader(`{"connection_id":"github-local"}`))
	request.Header.Set("Content-Type", jsonContentType)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated || scheduler.connectionID != "github-local" || scheduler.pullRequestID != "pr-1" {
		t.Fatalf("status=%d scheduler=%+v", response.Code, scheduler)
	}
	var body hydrationScheduleResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.JobID != "job-1" || !body.Created {
		t.Fatalf("body=%+v", body)
	}
}

func TestHydrationMutationRejectsUnsafeInputAndMapsTargets(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name        string
		path        string
		contentType string
		body        string
		err         error
		want        int
	}{
		{name: "missing connection", path: "/api/v1/mutate/pull-requests/pr-1/hydrate", contentType: jsonContentType, body: `{}`, want: http.StatusBadRequest},
		{name: "unknown field", path: "/api/v1/mutate/pull-requests/pr-1/hydrate", contentType: jsonContentType, body: `{"connection_id":"github-local","token":"no"}`, want: http.StatusBadRequest},
		{name: "bad content type", path: "/api/v1/mutate/pull-requests/pr-1/hydrate", contentType: "text/plain", body: `{"connection_id":"github-local"}`, want: http.StatusBadRequest},
		{name: "bad pull request", path: "/api/v1/mutate/pull-requests/%20/hydrate", contentType: jsonContentType, body: `{"connection_id":"github-local"}`, want: http.StatusBadRequest},
		{name: "not found", path: "/api/v1/mutate/pull-requests/pr-1/hydrate", contentType: jsonContentType, body: `{"connection_id":"github-local"}`, err: ErrHydrationTargetNotFound, want: http.StatusNotFound},
		{name: "unavailable", path: "/api/v1/mutate/pull-requests/pr-1/hydrate", contentType: jsonContentType, body: `{"connection_id":"github-local"}`, err: errors.New("database offline"), want: http.StatusServiceUnavailable},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := &fakeHydrationScheduler{err: testCase.err}
			handler := NewHydrationMutationHandler(HydrationMutationOptions{Scheduler: scheduler})
			request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
			request.Header.Set("Content-Type", testCase.contentType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != testCase.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if testCase.want == http.StatusBadRequest && scheduler.pullRequestID != "" {
				t.Fatalf("unsafe request called scheduler: %+v", scheduler)
			}
		})
	}
}
