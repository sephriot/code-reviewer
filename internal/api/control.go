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

// InboxReader supplies the two read-only operational projections exposed by
// the control API. It intentionally has no mutation or GitHub capabilities.
type InboxReader interface {
	ListCurrentAttention(context.Context, sqlite.AttentionQuery) (sqlite.AttentionPage, error)
	PullRequestTimeline(context.Context, sqlite.PullRequestTimelineQuery) (sqlite.PullRequestTimelinePage, error)
}

// ControlOptions identifies the reader and the configured local connection.
// Timeline IDs are scoped to a connection, so callers must provide it rather
// than allowing client input to select credentials or external connections.
type ControlOptions struct {
	Reader            InboxReader
	ProposalMutations ProposalMutationOptions
}

// NewControlHandler exposes health plus the read-only inbox and timeline.
// The unversioned paths are compatibility aliases for the v1 endpoints.
func NewControlHandler(readiness Readiness, options ControlOptions) http.Handler {
	mux := http.NewServeMux()
	registerHealthRoutes(mux, readiness)
	handler := controlHandler{reader: options.Reader}
	for _, path := range []string{
		"/api/v1/inbox",
		"/api/inbox",
	} {
		mux.HandleFunc("GET "+path, handler.inbox)
	}
	for _, path := range []string{
		"/api/v1/pull-requests/{id}/timeline",
		"/api/pull-requests/{id}/timeline",
	} {
		mux.HandleFunc("GET "+path, handler.timeline)
	}
	registerControlDashboard(mux)
	registerProposalMutationRoutes(mux, options.ProposalMutations)
	return mux
}

type controlHandler struct{ reader InboxReader }

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
