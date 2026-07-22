package api

import (
	"context"
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
