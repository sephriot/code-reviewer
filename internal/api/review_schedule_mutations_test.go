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

type fakeEligibleReviewScheduler struct {
	connectionID  string
	pullRequestID string
	result        EligibleReviewScheduleResult
	err           error
}

func (f *fakeEligibleReviewScheduler) ScheduleEligibleReview(_ context.Context, connectionID, pullRequestID string) (EligibleReviewScheduleResult, error) {
	f.connectionID, f.pullRequestID = connectionID, pullRequestID
	return f.result, f.err
}

func TestReviewSchedulingMutationQueuesEligibleAutomaticRule(t *testing.T) {
	t.Parallel()
	scheduler := &fakeEligibleReviewScheduler{result: EligibleReviewScheduleResult{Matched: true, RuleID: "rule-1", RuleVersionID: "version-1", TriggerKind: "automatic", RunID: "run-1", JobID: "job-1", Created: true}}
	handler := NewReviewSchedulingHandler(ReviewSchedulingOptions{Scheduler: scheduler})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/pull-requests/pr-1/schedule-review", strings.NewReader(`{"connection_id":"github-local"}`))
	request.Header.Set("Content-Type", jsonContentType)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || scheduler.connectionID != "github-local" || scheduler.pullRequestID != "pr-1" {
		t.Fatalf("status=%d scheduler=%+v", response.Code, scheduler)
	}
	var body reviewScheduleResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Matched || body.TriggerKind != "automatic" || body.JobID != "job-1" || !body.Created {
		t.Fatalf("body=%+v", body)
	}
}

func TestReviewSchedulingMutationReportsNoRuleAndSafeFailures(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		body string
		err  error
		want int
	}{
		{name: "no matching rule", body: `{"connection_id":"github-local"}`, want: http.StatusOK},
		{name: "evidence missing", body: `{"connection_id":"github-local"}`, err: ErrReviewEvidenceNotReady, want: http.StatusConflict},
		{name: "store failure", body: `{"connection_id":"github-local"}`, err: errors.New("offline"), want: http.StatusServiceUnavailable},
		{name: "unknown field", body: `{"connection_id":"github-local","token":"bad"}`, want: http.StatusBadRequest},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			scheduler := &fakeEligibleReviewScheduler{err: testCase.err}
			handler := NewReviewSchedulingHandler(ReviewSchedulingOptions{Scheduler: scheduler})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/pull-requests/pr-1/schedule-review", strings.NewReader(testCase.body))
			request.Header.Set("Content-Type", jsonContentType)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != testCase.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if testCase.want == http.StatusBadRequest && scheduler.pullRequestID != "" {
				t.Fatalf("invalid request called scheduler: %+v", scheduler)
			}
		})
	}

	disabled := NewReviewSchedulingHandler(ReviewSchedulingOptions{})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/mutate/pull-requests/pr-1/schedule-review", strings.NewReader(`{"connection_id":"github-local"}`))
	request.Header.Set("Content-Type", jsonContentType)
	response := httptest.NewRecorder()
	disabled.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled status=%d body=%s", response.Code, response.Body.String())
	}
}
