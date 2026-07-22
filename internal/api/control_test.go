package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

type fakeInboxReader struct {
	attention      sqlite.AttentionPage
	timeline       sqlite.PullRequestTimelinePage
	attentionQuery sqlite.AttentionQuery
	timelineQuery  sqlite.PullRequestTimelineQuery
	analytics      sqlite.AnalyticsOverview
	analyticsCalls int
	err            error
}

func (f *fakeInboxReader) ListCurrentAttention(_ context.Context, query sqlite.AttentionQuery) (sqlite.AttentionPage, error) {
	f.attentionQuery = query
	return f.attention, f.err
}

func (f *fakeInboxReader) PullRequestTimeline(_ context.Context, query sqlite.PullRequestTimelineQuery) (sqlite.PullRequestTimelinePage, error) {
	f.timelineQuery = query
	return f.timeline, f.err
}

func (f *fakeInboxReader) AnalyticsOverview(_ context.Context) (sqlite.AnalyticsOverview, error) {
	f.analyticsCalls++
	return f.analytics, f.err
}

func TestControlReadEndpointsAndAliases(t *testing.T) {
	t.Parallel()
	reader := &fakeInboxReader{
		attention: sqlite.AttentionPage{Items: []sqlite.AttentionItem{{
			Kind: sqlite.AttentionKindFailedRun, ID: "run-1", ConnectionID: "connection-1", PullRequestID: "pr-1", RevisionID: "revision-1", ObservationID: "observation-1", OccurredAt: time.Unix(1, 0).UTC(), State: sqlite.TimelineStateCurrent, Current: true, Detail: "failed_terminal",
		}}, NextCursor: "next"},
		timeline: sqlite.PullRequestTimelinePage{Items: []sqlite.TimelineItem{{
			Kind: sqlite.TimelineKindRun, ID: "run-1", ConnectionID: "connection-1", PullRequestID: "pr-1", RevisionID: "revision-1", ObservationID: "observation-1", OccurredAt: time.Unix(2, 0).UTC(), State: sqlite.TimelineStateCurrent, Current: true, Detail: "cli",
		}}},
	}
	handler := NewControlHandler(Readiness{}, ControlOptions{Reader: reader})

	for _, path := range []string{"/api/v1/inbox?limit=2&cursor=abc", "/api/inbox?limit=2&cursor=abc"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != jsonContentType || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("inbox %q response = %d headers=%v", path, response.Code, response.Header())
		}
		if reader.attentionQuery.Limit != 2 || reader.attentionQuery.Cursor != "abc" {
			t.Fatalf("inbox query = %+v", reader.attentionQuery)
		}
	}
	for _, path := range []string{"/api/v1/pull-requests/pr-1/timeline?connection_id=connection-1&limit=3&cursor=def", "/api/pull-requests/pr-1/timeline?connection_id=connection-1&limit=3&cursor=def"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != jsonContentType {
			t.Fatalf("timeline %q response = %d headers=%v", path, response.Code, response.Header())
		}
		if reader.timelineQuery.ConnectionID != "connection-1" || reader.timelineQuery.PullRequestID != "pr-1" || reader.timelineQuery.Limit != 3 || reader.timelineQuery.Cursor != "def" {
			t.Fatalf("timeline query = %+v", reader.timelineQuery)
		}
	}
}

func TestControlRejectsInvalidInputAndDoesNotRead(t *testing.T) {
	t.Parallel()
	reader := &fakeInboxReader{}
	handler := NewControlHandler(Readiness{}, ControlOptions{Reader: reader})
	for _, path := range []string{
		"/api/v1/inbox?limit=0", "/api/v1/inbox?limit=101", "/api/v1/inbox?limit=1&limit=2", "/api/v1/inbox?unknown=value",
		"/api/v1/pull-requests/pr-1/timeline", "/api/v1/pull-requests/pr-1/timeline?connection_id=one&connection_id=two", "/api/v1/pull-requests/pr-1/timeline?connection_id=one&limit=nope",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Errorf("%q status = %d, want 400", path, response.Code)
		}
	}
	if reader.attentionQuery != (sqlite.AttentionQuery{}) || reader.timelineQuery != (sqlite.PullRequestTimelineQuery{}) {
		t.Fatalf("invalid requests called reader: %+v %+v", reader.attentionQuery, reader.timelineQuery)
	}
}

func TestControlReadFailureAndUnsupportedMethod(t *testing.T) {
	t.Parallel()
	handler := NewControlHandler(Readiness{}, ControlOptions{Reader: &fakeInboxReader{err: errors.New("database unavailable")}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/inbox", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("read error status = %d", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/inbox", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", response.Code)
	}
}

func TestControlAnalyticsOverviewIsLoopbackReadOnlyAndStructured(t *testing.T) {
	t.Parallel()
	reader := &fakeInboxReader{analytics: sqlite.AnalyticsOverview{
		ObservedPullRequests: 2, ReviewRuns: 3, Assessments: 4, PolicyEvaluations: 5,
		HumanReviewEvaluations: 1, Proposals: 6, ProposalRevisions: 7, ProposalApprovals: 2,
		ProposalRejections: 3, PublicationEffects: 8, PublicationAttempts: 9,
		SimulatedPublicationAttempts: 4, SuccessfulPublicationAttempts: 2,
		RetryablePublicationFailures: 1, TerminalPublicationFailures: 1, UncertainPublicationAttempts: 1,
	}}
	handler := NewControlHandler(Readiness{}, ControlOptions{Reader: reader})

	remote := httptest.NewRequest(http.MethodGet, "/api/v1/analytics/overview", nil)
	remote.RemoteAddr = "198.51.100.7:443"
	remoteResponse := httptest.NewRecorder()
	handler.ServeHTTP(remoteResponse, remote)
	if remoteResponse.Code != http.StatusForbidden || reader.analyticsCalls != 0 {
		t.Fatalf("remote status=%d calls=%d", remoteResponse.Code, reader.analyticsCalls)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/analytics/overview", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != jsonContentType || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("analytics response=%d headers=%v", response.Code, response.Header())
	}
	var body struct {
		ObservedPullRequests int `json:"observed_pull_requests"`
		Reviews              struct {
			Runs int `json:"runs"`
		} `json:"reviews"`
		Proposals struct {
			Rejected int `json:"rejected"`
		} `json:"proposals"`
		Publications struct {
			FailedTerminal int `json:"failed_terminal"`
		} `json:"publications"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ObservedPullRequests != 2 || body.Reviews.Runs != 3 || body.Proposals.Rejected != 3 || body.Publications.FailedTerminal != 1 || reader.analyticsCalls != 1 {
		t.Fatalf("analytics body=%+v calls=%d", body, reader.analyticsCalls)
	}

	invalid := httptest.NewRequest(http.MethodGet, "/api/v1/analytics/overview?limit=1", nil)
	invalid.RemoteAddr = "127.0.0.1:1234"
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest || reader.analyticsCalls != 1 {
		t.Fatalf("invalid status=%d calls=%d", invalidResponse.Code, reader.analyticsCalls)
	}
}

func TestControlServesReadOnlyDashboard(t *testing.T) {
	t.Parallel()
	reader := &fakeInboxReader{}
	handler := NewControlHandler(Readiness{}, ControlOptions{Reader: reader})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d", response.Code)
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("dashboard content type = %q", response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	for _, text := range []string{"Control Desk", "/api/v1/inbox", "timeline"} {
		if !strings.Contains(body, text) {
			t.Errorf("dashboard missing %q", text)
		}
	}
	if reader.attentionQuery != (sqlite.AttentionQuery{}) || reader.timelineQuery != (sqlite.PullRequestTimelineQuery{}) {
		t.Fatalf("dashboard called read model: attention=%+v timeline=%+v", reader.attentionQuery, reader.timelineQuery)
	}
}
