package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

const maxControlPageLimit = 100

// ControlReader supplies read-only operational and analytics projections for
// the control API. It intentionally has no mutation or GitHub capabilities.
type ControlReader interface {
	ListCurrentAttention(context.Context, sqlite.AttentionQuery) (sqlite.AttentionPage, error)
	ListHistory(context.Context, sqlite.HistoryQuery) (sqlite.HistoryPage, error)
	PullRequestTimeline(context.Context, sqlite.PullRequestTimelineQuery) (sqlite.PullRequestTimelinePage, error)
	PullRequestDetail(context.Context, sqlite.PullRequestDetailQuery) (sqlite.PullRequestDetail, error)
	AnalyticsOverview(context.Context) (sqlite.AnalyticsOverview, error)
	SettingsSummary(context.Context) (sqlite.SettingsSummary, error)
}

// ControlOptions identifies the reader and the configured local connection.
// Timeline IDs are scoped to a connection, so callers must provide it rather
// than allowing client input to select credentials or external connections.
type ControlOptions struct {
	Reader               ControlReader
	ProposalMutations    ProposalMutationOptions
	PublicationMutations PublicationMutationOptions
}

// NewControlHandler exposes health plus the read-only inbox and timeline.
// The unversioned paths are compatibility aliases for the v1 endpoints.
func NewControlHandler(readiness Readiness, options ControlOptions) http.Handler {
	mux := http.NewServeMux()
	registerHealthRoutes(mux, readiness)
	handler := controlHandler{reader: options.Reader, schemaStatus: readiness.SchemaStatus}
	for _, path := range []string{
		"/api/v1/inbox",
		"/api/inbox",
	} {
		mux.HandleFunc("GET "+path, handler.inbox)
	}
	for _, path := range []string{
		"/api/v1/history",
		"/api/history",
	} {
		mux.HandleFunc("GET "+path, handler.history)
	}
	for _, path := range []string{
		"/api/v1/pull-requests/{id}/timeline",
		"/api/pull-requests/{id}/timeline",
	} {
		mux.HandleFunc("GET "+path, handler.timeline)
	}
	for _, path := range []string{
		"/api/v1/pull-requests/{id}",
		"/api/pull-requests/{id}",
	} {
		mux.HandleFunc("GET "+path, handler.pullRequestDetail)
	}
	mux.HandleFunc("GET /api/v1/analytics/overview", handler.analyticsOverview)
	mux.HandleFunc("GET /api/v1/settings", handler.settings)
	registerControlDashboard(mux)
	registerProposalMutationRoutes(mux, options.ProposalMutations)
	registerPublicationMutationRoutes(mux, options.PublicationMutations)
	return mux
}

type controlHandler struct {
	reader       ControlReader
	schemaStatus func(context.Context) (SchemaStatus, error)
}

type pageParams struct {
	limit  int
	cursor string
}

type apiErrorResponse struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type pageResponse[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type attentionResponse struct {
	Kind          sqlite.AttentionKind `json:"kind"`
	ID            string               `json:"id"`
	ConnectionID  string               `json:"connection_id"`
	PullRequestID string               `json:"pull_request_id"`
	RevisionID    string               `json:"revision_id"`
	ObservationID string               `json:"observation_id"`
	OccurredAt    time.Time            `json:"occurred_at"`
	State         sqlite.TimelineState `json:"state"`
	Current       bool                 `json:"current"`
	Detail        string               `json:"detail"`
}

type timelineResponse struct {
	Kind          sqlite.TimelineKind  `json:"kind"`
	ID            string               `json:"id"`
	ConnectionID  string               `json:"connection_id"`
	PullRequestID string               `json:"pull_request_id"`
	RevisionID    string               `json:"revision_id"`
	ObservationID string               `json:"observation_id"`
	OccurredAt    time.Time            `json:"occurred_at"`
	State         sqlite.TimelineState `json:"state"`
	Current       bool                 `json:"current"`
	Detail        string               `json:"detail"`
}

type pullRequestDetailResponse struct {
	ConnectionID  string `json:"connection_id"`
	RepositoryID  string `json:"repository_id"`
	PullRequestID string `json:"pull_request_id"`
	Owner         string `json:"owner"`
	Repository    string `json:"repository"`
	Number        int    `json:"number"`
	Title         string `json:"title"`
	State         string `json:"state"`
	HTMLURL       string `json:"html_url"`
	Freshness     string `json:"freshness"`

	CurrentRevision struct {
		ID           string `json:"id"`
		IdentityKind string `json:"identity_kind"`
		HeadSHA      string `json:"head_sha"`
		BaseSHA      string `json:"base_sha"`
	} `json:"current_revision"`
	CurrentObservation struct {
		ID         string    `json:"id"`
		ObservedAt time.Time `json:"observed_at"`
	} `json:"current_observation"`
	CurrentCounts struct {
		ReviewRuns        int `json:"review_runs"`
		ProposalRevisions int `json:"proposal_revisions"`
	} `json:"current_counts"`
}

type historyResponse struct {
	Kind          sqlite.HistoryKind   `json:"kind"`
	ID            string               `json:"id"`
	ConnectionID  string               `json:"connection_id"`
	PullRequestID string               `json:"pull_request_id"`
	RevisionID    string               `json:"revision_id"`
	ObservationID string               `json:"observation_id"`
	OccurredAt    time.Time            `json:"occurred_at"`
	State         sqlite.TimelineState `json:"state"`
	Current       bool                 `json:"current"`
	Detail        string               `json:"detail"`
}

type analyticsOverviewResponse struct {
	ObservedPullRequests int `json:"observed_pull_requests"`
	Reviews              struct {
		Runs        int `json:"runs"`
		Assessments int `json:"assessments"`
	} `json:"reviews"`
	Policy struct {
		Evaluations        int `json:"evaluations"`
		RequireHumanReview int `json:"require_human_review"`
	} `json:"policy"`
	Proposals struct {
		Total     int `json:"total"`
		Revisions int `json:"revisions"`
		Approved  int `json:"approved"`
		Rejected  int `json:"rejected"`
	} `json:"proposals"`
	Publications struct {
		Effects           int `json:"effects"`
		Attempts          int `json:"attempts"`
		Simulated         int `json:"simulated"`
		Succeeded         int `json:"succeeded"`
		FailedRetryable   int `json:"failed_retryable"`
		FailedTerminal    int `json:"failed_terminal"`
		UncertainDelivery int `json:"uncertain_delivery"`
	} `json:"publications"`
}

type settingsResponse struct {
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

func (h controlHandler) inbox(response http.ResponseWriter, request *http.Request) {
	params, err := parsePageParams(request, false)
	if err != nil {
		writeControlError(response, http.StatusBadRequest, "invalid_request", err.Error(), false)
		return
	}
	if h.reader == nil {
		writeControlError(response, http.StatusServiceUnavailable, "read_model_unavailable", "control read model is unavailable", true)
		return
	}
	page, err := h.reader.ListCurrentAttention(request.Context(), sqlite.AttentionQuery{Limit: params.limit, Cursor: params.cursor})
	if err != nil {
		handleReadError(response, err)
		return
	}
	items := make([]attentionResponse, len(page.Items))
	for index, item := range page.Items {
		items[index] = attentionResponse{item.Kind, item.ID, item.ConnectionID, item.PullRequestID, item.RevisionID, item.ObservationID, item.OccurredAt, item.State, item.Current, item.Detail}
	}
	writeControlJSON(response, http.StatusOK, pageResponse[attentionResponse]{Items: items, NextCursor: page.NextCursor})
}

func (h controlHandler) history(response http.ResponseWriter, request *http.Request) {
	if !isLoopbackRemoteAddress(request.RemoteAddr) {
		writeControlError(response, http.StatusForbidden, "loopback_required", "history is available only on loopback", false)
		return
	}
	params, err := parsePageParams(request, true)
	if err != nil {
		writeControlError(response, http.StatusBadRequest, "invalid_request", err.Error(), false)
		return
	}
	connectionID := strings.TrimSpace(request.URL.Query().Get("connection_id"))
	if len(connectionID) > 256 {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "connection_id is invalid", false)
		return
	}
	if h.reader == nil {
		writeControlError(response, http.StatusServiceUnavailable, "read_model_unavailable", "control read model is unavailable", true)
		return
	}
	page, err := h.reader.ListHistory(request.Context(), sqlite.HistoryQuery{
		ConnectionID: connectionID, Limit: params.limit, Cursor: params.cursor,
	})
	if err != nil {
		handleReadError(response, err)
		return
	}
	items := make([]historyResponse, len(page.Items))
	for index, item := range page.Items {
		items[index] = historyResponse{item.Kind, item.ID, item.ConnectionID, item.PullRequestID, item.RevisionID, item.ObservationID, item.OccurredAt, item.State, item.Current, item.Detail}
	}
	writeControlJSON(response, http.StatusOK, pageResponse[historyResponse]{Items: items, NextCursor: page.NextCursor})
}

func (h controlHandler) timeline(response http.ResponseWriter, request *http.Request) {
	params, err := parsePageParams(request, true)
	if err != nil {
		writeControlError(response, http.StatusBadRequest, "invalid_request", err.Error(), false)
		return
	}
	pullRequestID := strings.TrimSpace(request.PathValue("id"))
	if pullRequestID == "" || len(pullRequestID) > 256 {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "pull request ID is invalid", false)
		return
	}
	connectionID := strings.TrimSpace(request.URL.Query().Get("connection_id"))
	if connectionID == "" || len(connectionID) > 256 {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "connection_id is required", false)
		return
	}
	if h.reader == nil {
		writeControlError(response, http.StatusServiceUnavailable, "read_model_unavailable", "control read model is unavailable", true)
		return
	}
	page, err := h.reader.PullRequestTimeline(request.Context(), sqlite.PullRequestTimelineQuery{
		ConnectionID: connectionID, PullRequestID: pullRequestID, Limit: params.limit, Cursor: params.cursor,
	})
	if err != nil {
		handleReadError(response, err)
		return
	}
	items := make([]timelineResponse, len(page.Items))
	for index, item := range page.Items {
		items[index] = timelineResponse{item.Kind, item.ID, item.ConnectionID, item.PullRequestID, item.RevisionID, item.ObservationID, item.OccurredAt, item.State, item.Current, item.Detail}
	}
	writeControlJSON(response, http.StatusOK, pageResponse[timelineResponse]{Items: items, NextCursor: page.NextCursor})
}

func (h controlHandler) pullRequestDetail(response http.ResponseWriter, request *http.Request) {
	if !isLoopbackRemoteAddress(request.RemoteAddr) {
		writeControlError(response, http.StatusForbidden, "loopback_required", "pull request detail is available only on loopback", false)
		return
	}
	pullRequestID := strings.TrimSpace(request.PathValue("id"))
	if pullRequestID == "" || len(pullRequestID) > 256 {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "pull request ID is invalid", false)
		return
	}
	connectionID, err := parseDetailConnectionID(request)
	if err != nil {
		writeControlError(response, http.StatusBadRequest, "invalid_request", err.Error(), false)
		return
	}
	if h.reader == nil {
		writeControlError(response, http.StatusServiceUnavailable, "read_model_unavailable", "control read model is unavailable", true)
		return
	}
	detail, err := h.reader.PullRequestDetail(request.Context(), sqlite.PullRequestDetailQuery{
		ConnectionID: connectionID, PullRequestID: pullRequestID,
	})
	if err != nil {
		handleReadError(response, err)
		return
	}
	writeControlJSON(response, http.StatusOK, newPullRequestDetailResponse(detail))
}

func parseDetailConnectionID(request *http.Request) (string, error) {
	values := request.URL.Query()
	if len(values) != 1 || len(values["connection_id"]) != 1 {
		return "", errors.New("connection_id is required")
	}
	connectionID := strings.TrimSpace(values.Get("connection_id"))
	if connectionID == "" || len(connectionID) > 256 {
		return "", errors.New("connection_id is invalid")
	}
	return connectionID, nil
}

func newPullRequestDetailResponse(detail sqlite.PullRequestDetail) pullRequestDetailResponse {
	response := pullRequestDetailResponse{
		ConnectionID: detail.ConnectionID, RepositoryID: detail.RepositoryID, PullRequestID: detail.PullRequestID,
		Owner: detail.Owner, Repository: detail.Repository, Number: detail.Number, Title: detail.Title,
		State: detail.State, HTMLURL: detail.HTMLURL, Freshness: detail.Freshness,
	}
	response.CurrentRevision.ID, response.CurrentRevision.IdentityKind = detail.CurrentRevisionID, detail.CurrentRevisionIdentityKind
	response.CurrentRevision.HeadSHA, response.CurrentRevision.BaseSHA = detail.CurrentHeadSHA, detail.CurrentBaseSHA
	response.CurrentObservation.ID, response.CurrentObservation.ObservedAt = detail.CurrentObservationID, detail.CurrentObservedAt
	response.CurrentCounts.ReviewRuns, response.CurrentCounts.ProposalRevisions = detail.CurrentReviewRunCount, detail.CurrentProposalRevisionCount
	return response
}

func (h controlHandler) analyticsOverview(response http.ResponseWriter, request *http.Request) {
	if !isLoopbackRemoteAddress(request.RemoteAddr) {
		writeControlError(response, http.StatusForbidden, "loopback_required", "analytics overview is available only on loopback", false)
		return
	}
	if request.URL.RawQuery != "" {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "analytics overview does not accept query parameters", false)
		return
	}
	if h.reader == nil {
		writeControlError(response, http.StatusServiceUnavailable, "read_model_unavailable", "control read model is unavailable", true)
		return
	}
	overview, err := h.reader.AnalyticsOverview(request.Context())
	if err != nil {
		handleReadError(response, err)
		return
	}
	writeControlJSON(response, http.StatusOK, newAnalyticsOverviewResponse(overview))
}

func (h controlHandler) settings(response http.ResponseWriter, request *http.Request) {
	if !isLoopbackRemoteAddress(request.RemoteAddr) {
		writeControlError(response, http.StatusForbidden, "loopback_required", "settings are available only on loopback", false)
		return
	}
	if request.URL.RawQuery != "" {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "settings do not accept query parameters", false)
		return
	}
	if h.reader == nil {
		writeControlError(response, http.StatusServiceUnavailable, "read_model_unavailable", "control read model is unavailable", true)
		return
	}
	summary, err := h.reader.SettingsSummary(request.Context())
	if err != nil {
		handleReadError(response, err)
		return
	}
	if h.schemaStatus == nil {
		writeControlError(response, http.StatusServiceUnavailable, "schema_status_unavailable", "schema status is unavailable", true)
		return
	}
	status, err := h.schemaStatus(request.Context())
	if err != nil || !validSchemaStatus(status) {
		writeControlError(response, http.StatusServiceUnavailable, "schema_status_unavailable", "schema status is unavailable", true)
		return
	}
	result := settingsResponse{PublicationMode: string(summary.PublicationMode)}
	result.Configured.ActiveRules, result.Configured.Profiles = summary.ActiveWatchRules, summary.ConfiguredProfiles
	result.Schema.Current = status.Pending == 0 && status.Current == status.Latest
	result.Schema.Version, result.Schema.Latest, result.Schema.Pending = status.Current, status.Latest, status.Pending
	writeControlJSON(response, http.StatusOK, result)
}

func newAnalyticsOverviewResponse(overview sqlite.AnalyticsOverview) analyticsOverviewResponse {
	response := analyticsOverviewResponse{ObservedPullRequests: overview.ObservedPullRequests}
	response.Reviews.Runs, response.Reviews.Assessments = overview.ReviewRuns, overview.Assessments
	response.Policy.Evaluations, response.Policy.RequireHumanReview = overview.PolicyEvaluations, overview.HumanReviewEvaluations
	response.Proposals.Total, response.Proposals.Revisions = overview.Proposals, overview.ProposalRevisions
	response.Proposals.Approved, response.Proposals.Rejected = overview.ProposalApprovals, overview.ProposalRejections
	response.Publications.Effects, response.Publications.Attempts = overview.PublicationEffects, overview.PublicationAttempts
	response.Publications.Simulated, response.Publications.Succeeded = overview.SimulatedPublicationAttempts, overview.SuccessfulPublicationAttempts
	response.Publications.FailedRetryable, response.Publications.FailedTerminal = overview.RetryablePublicationFailures, overview.TerminalPublicationFailures
	response.Publications.UncertainDelivery = overview.UncertainPublicationAttempts
	return response
}

func parsePageParams(request *http.Request, timeline bool) (pageParams, error) {
	allowed := map[string]bool{"limit": true, "cursor": true}
	if timeline {
		allowed["connection_id"] = true
	}
	values := request.URL.Query()
	for key, valuesForKey := range values {
		if !allowed[key] || len(valuesForKey) != 1 {
			return pageParams{}, errors.New("query parameters are invalid")
		}
	}
	params := pageParams{cursor: values.Get("cursor")}
	if rawLimit := values.Get("limit"); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > maxControlPageLimit {
			return pageParams{}, fmt.Errorf("limit must be between 1 and %d", maxControlPageLimit)
		}
		params.limit = limit
	} else if _, present := values["limit"]; present {
		return pageParams{}, errors.New("limit must be between 1 and 100")
	}
	if len(params.cursor) > 512 {
		return pageParams{}, errors.New("cursor is invalid")
	}
	return params, nil
}

func handleReadError(response http.ResponseWriter, err error) {
	if errors.Is(err, sqlite.ErrPullRequestDetailNotFound) {
		writeControlError(response, http.StatusNotFound, "not_found", "pull request was not found", false)
		return
	}
	if strings.Contains(err.Error(), "read page cursor is invalid") || strings.Contains(err.Error(), "read page limit") || strings.Contains(err.Error(), "timeline connection") {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "pagination or resource parameters are invalid", false)
		return
	}
	writeControlError(response, http.StatusInternalServerError, "read_failed", "could not read control-plane state", true)
}

func writeControlError(response http.ResponseWriter, status int, code, message string, retryable bool) {
	writeControlJSON(response, status, apiErrorResponse{Error: apiError{Code: code, Message: message, Retryable: retryable}})
}

func writeControlJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", jsonContentType)
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}
