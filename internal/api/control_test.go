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
	pullRequests   sqlite.PullRequestListPage
	timeline       sqlite.PullRequestTimelinePage
	detail         sqlite.PullRequestDetail
	history        sqlite.HistoryPage
	attentionQuery sqlite.AttentionQuery
	timelineQuery  sqlite.PullRequestTimelineQuery
	detailQuery    sqlite.PullRequestDetailQuery
	historyQuery   sqlite.HistoryQuery
	analytics      sqlite.AnalyticsOverview
	analyticsCalls int
	settings       sqlite.SettingsSummary
	settingsCalls  int
	status         sqlite.PublicationEffectStatus
	statusEffectID string
	statusErr      error
	err            error
}

func (f *fakeInboxReader) ListPullRequests(_ context.Context, _ sqlite.PullRequestListQuery) (sqlite.PullRequestListPage, error) {
	return f.pullRequests, f.err
}

func (f *fakeInboxReader) ListCurrentAttention(_ context.Context, query sqlite.AttentionQuery) (sqlite.AttentionPage, error) {
	f.attentionQuery = query
	return f.attention, f.err
}

func (f *fakeInboxReader) ListHistory(_ context.Context, query sqlite.HistoryQuery) (sqlite.HistoryPage, error) {
	f.historyQuery = query
	return f.history, f.err
}

func (f *fakeInboxReader) PullRequestTimeline(_ context.Context, query sqlite.PullRequestTimelineQuery) (sqlite.PullRequestTimelinePage, error) {
	f.timelineQuery = query
	return f.timeline, f.err
}

func (f *fakeInboxReader) PullRequestDetail(_ context.Context, query sqlite.PullRequestDetailQuery) (sqlite.PullRequestDetail, error) {
	f.detailQuery = query
	return f.detail, f.err
}

func (f *fakeInboxReader) AnalyticsOverview(_ context.Context) (sqlite.AnalyticsOverview, error) {
	f.analyticsCalls++
	return f.analytics, f.err
}

func (f *fakeInboxReader) SettingsSummary(_ context.Context) (sqlite.SettingsSummary, error) {
	f.settingsCalls++
	return f.settings, f.err
}

func (f *fakeInboxReader) PublicationEffectStatus(_ context.Context, effectID string) (sqlite.PublicationEffectStatus, error) {
	f.statusEffectID = effectID
	return f.status, f.statusErr
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
		detail: sqlite.PullRequestDetail{
			ConnectionID: "connection-1", RepositoryID: "repo-1", PullRequestID: "pr-1", Owner: "owner", Repository: "repository", Number: 1,
			Title: "Detail", State: "open", Freshness: "fresh", CurrentRevisionID: "revision-1", CurrentObservationID: "observation-1", CurrentObservedAt: time.Unix(2, 0).UTC(),
		},
		history: sqlite.HistoryPage{Items: []sqlite.HistoryItem{{
			Kind: sqlite.HistoryKindCompletedRun, ID: "run-1", ConnectionID: "connection-1", PullRequestID: "pr-1", RevisionID: "revision-1", ObservationID: "observation-1", OccurredAt: time.Unix(3, 0).UTC(), State: sqlite.TimelineStateCurrent, Current: true, Detail: "cli:succeeded",
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
	for _, path := range []string{"/api/v1/history?connection_id=connection-1&limit=4&cursor=ghi", "/api/history?connection_id=connection-1&limit=4&cursor=ghi"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "127.0.0.1:1234"
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != jsonContentType || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("history %q response = %d headers=%v", path, response.Code, response.Header())
		}
		if reader.historyQuery.ConnectionID != "connection-1" || reader.historyQuery.Limit != 4 || reader.historyQuery.Cursor != "ghi" {
			t.Fatalf("history query = %+v", reader.historyQuery)
		}
	}
	for _, path := range []string{"/api/v1/pull-requests/pr-1?connection_id=connection-1", "/api/pull-requests/pr-1?connection_id=connection-1"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "127.0.0.1:1234"
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != jsonContentType || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("detail %q response = %d headers=%v", path, response.Code, response.Header())
		}
		if reader.detailQuery.ConnectionID != "connection-1" || reader.detailQuery.PullRequestID != "pr-1" {
			t.Fatalf("detail query = %+v", reader.detailQuery)
		}
	}
}

func TestPublicationEffectStatusIsLoopbackOnlyAndSafe(t *testing.T) {
	t.Parallel()
	reader := &fakeInboxReader{status: sqlite.PublicationEffectStatus{
		EffectID: "effect-1", PublicationMode: sqlite.PublicationModeEnabled,
		Attempt: &sqlite.PublicationAttemptStatus{
			AttemptID: "attempt-1", PublicationMode: sqlite.PublicationModeEnabled,
			Outcome: "uncertain", CompletedAt: time.Unix(10, 0).UTC(),
		},
	}}
	handler := NewControlHandler(Readiness{}, ControlOptions{PublicationStatuses: PublicationEffectStatusOptions{Reader: reader}})
	remote := httptest.NewRequest(http.MethodGet, "/api/v1/publication-effects/effect-1", nil)
	remote.RemoteAddr = "198.51.100.7:443"
	remoteResponse := httptest.NewRecorder()
	handler.ServeHTTP(remoteResponse, remote)
	if remoteResponse.Code != http.StatusForbidden || reader.statusEffectID != "" {
		t.Fatalf("remote status=%d effect=%q", remoteResponse.Code, reader.statusEffectID)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/publication-effects/effect-1", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || reader.statusEffectID != "effect-1" || !strings.Contains(response.Body.String(), "uncertain") {
		t.Fatalf("status=%d effect=%q body=%s", response.Code, reader.statusEffectID, response.Body.String())
	}
}

func TestControlRejectsInvalidInputAndDoesNotRead(t *testing.T) {
	t.Parallel()
	reader := &fakeInboxReader{}
	handler := NewControlHandler(Readiness{}, ControlOptions{Reader: reader})
	for _, path := range []string{
		"/api/v1/inbox?limit=0", "/api/v1/inbox?limit=101", "/api/v1/inbox?limit=1&limit=2", "/api/v1/inbox?unknown=value",
		"/api/v1/pull-requests/pr-1/timeline", "/api/v1/pull-requests/pr-1/timeline?connection_id=one&connection_id=two", "/api/v1/pull-requests/pr-1/timeline?connection_id=one&limit=nope",
		"/api/v1/pull-requests/pr-1", "/api/v1/pull-requests/pr-1?connection_id=one&connection_id=two", "/api/v1/pull-requests/pr-1?connection_id=one&limit=1",
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "127.0.0.1:1234"
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%q status = %d, want 400", path, response.Code)
		}
	}
	if reader.attentionQuery != (sqlite.AttentionQuery{}) || reader.timelineQuery != (sqlite.PullRequestTimelineQuery{}) || reader.detailQuery != (sqlite.PullRequestDetailQuery{}) || reader.historyQuery != (sqlite.HistoryQuery{}) {
		t.Fatalf("invalid requests called reader: %+v %+v %+v %+v", reader.attentionQuery, reader.timelineQuery, reader.detailQuery, reader.historyQuery)
	}
}

func TestControlPullRequestDetailRequiresLoopbackAndMapsNotFound(t *testing.T) {
	t.Parallel()
	reader := &fakeInboxReader{err: sqlite.ErrPullRequestDetailNotFound}
	handler := NewControlHandler(Readiness{}, ControlOptions{Reader: reader})

	remote := httptest.NewRequest(http.MethodGet, "/api/v1/pull-requests/pr-1?connection_id=connection-1", nil)
	remote.RemoteAddr = "198.51.100.7:443"
	remoteResponse := httptest.NewRecorder()
	handler.ServeHTTP(remoteResponse, remote)
	if remoteResponse.Code != http.StatusForbidden || reader.detailQuery != (sqlite.PullRequestDetailQuery{}) {
		t.Fatalf("remote detail status=%d query=%+v", remoteResponse.Code, reader.detailQuery)
	}

	local := httptest.NewRequest(http.MethodGet, "/api/v1/pull-requests/pr-1?connection_id=connection-1", nil)
	local.RemoteAddr = "127.0.0.1:1234"
	localResponse := httptest.NewRecorder()
	handler.ServeHTTP(localResponse, local)
	if localResponse.Code != http.StatusNotFound {
		t.Fatalf("missing detail status=%d", localResponse.Code)
	}
}

func TestControlHistoryRequiresLoopbackButNoAuthentication(t *testing.T) {
	t.Parallel()
	reader := &fakeInboxReader{}
	handler := NewControlHandler(Readiness{}, ControlOptions{Reader: reader})

	remote := httptest.NewRequest(http.MethodGet, "/api/v1/history", nil)
	remote.RemoteAddr = "198.51.100.7:443"
	remoteResponse := httptest.NewRecorder()
	handler.ServeHTTP(remoteResponse, remote)
	if remoteResponse.Code != http.StatusForbidden || reader.historyQuery != (sqlite.HistoryQuery{}) {
		t.Fatalf("remote status=%d query=%+v", remoteResponse.Code, reader.historyQuery)
	}

	for _, path := range []string{
		"/api/v1/history?limit=0", "/api/v1/history?connection_id=one&connection_id=two", "/api/v1/history?unknown=value",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "127.0.0.1:1234"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%q status=%d, want 400", path, response.Code)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/history", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || reader.historyQuery != (sqlite.HistoryQuery{}) {
		t.Fatalf("loopback status=%d query=%+v", response.Code, reader.historyQuery)
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
	raw := response.Body.String()
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&body); err != nil {
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

func TestControlSettingsIsLoopbackReadOnlyAndSafe(t *testing.T) {
	t.Parallel()
	reader := &fakeInboxReader{settings: sqlite.SettingsSummary{
		PublicationMode:    sqlite.PublicationModeSimulated,
		ActiveWatchRules:   2,
		ConfiguredProfiles: 3,
	}}
	readiness := Readiness{SchemaStatus: func(context.Context) (SchemaStatus, error) {
		return SchemaStatus{Current: 8, Latest: 8, Pending: 0}, nil
	}}
	handler := NewControlHandler(readiness, ControlOptions{Reader: reader})

	remote := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	remote.RemoteAddr = "198.51.100.7:443"
	remoteResponse := httptest.NewRecorder()
	handler.ServeHTTP(remoteResponse, remote)
	if remoteResponse.Code != http.StatusForbidden || reader.settingsCalls != 0 {
		t.Fatalf("remote status=%d calls=%d", remoteResponse.Code, reader.settingsCalls)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	request.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != jsonContentType || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("settings response=%d headers=%v", response.Code, response.Header())
	}
	var body struct {
		PublicationMode string `json:"publication_mode"`
		Configured      struct {
			ActiveRules int `json:"active_rules"`
			Profiles    int `json:"profiles"`
		} `json:"configured"`
		Schema struct {
			Current bool `json:"current"`
			Version int  `json:"version"`
			Latest  int  `json:"latest"`
			Pending int  `json:"pending"`
		} `json:"schema"`
	}
	raw := response.Body.String()
	if err := json.NewDecoder(strings.NewReader(raw)).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.PublicationMode != "simulated" || body.Configured.ActiveRules != 2 || body.Configured.Profiles != 3 ||
		!body.Schema.Current || body.Schema.Version != 8 || body.Schema.Latest != 8 || body.Schema.Pending != 0 || reader.settingsCalls != 1 {
		t.Fatalf("settings body=%+v calls=%d", body, reader.settingsCalls)
	}
	if strings.Contains(raw, "token") || strings.Contains(raw, "credential") {
		t.Fatal("settings response exposed secret-shaped fields")
	}

	invalid := httptest.NewRequest(http.MethodGet, "/api/v1/settings?limit=1", nil)
	invalid.RemoteAddr = "127.0.0.1:1234"
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest || reader.settingsCalls != 1 {
		t.Fatalf("invalid status=%d calls=%d", invalidResponse.Code, reader.settingsCalls)
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
	for _, text := range []string{"Control Desk", "/api/v1/inbox", "timeline", "/api/v1/notification-preferences", "Save local preferences"} {
		if !strings.Contains(body, text) {
			t.Errorf("dashboard missing %q", text)
		}
	}
	if reader.attentionQuery != (sqlite.AttentionQuery{}) || reader.timelineQuery != (sqlite.PullRequestTimelineQuery{}) {
		t.Fatalf("dashboard called read model: attention=%+v timeline=%+v", reader.attentionQuery, reader.timelineQuery)
	}
}
