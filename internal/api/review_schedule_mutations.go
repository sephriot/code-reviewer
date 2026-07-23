package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ErrReviewEvidenceNotReady means current verified canonical evidence is
// absent, stale, or cannot safely select a review rule.
var ErrReviewEvidenceNotReady = errors.New("review evidence is not ready")

// EligibleReviewScheduleResult reports local automatic-rule selection and any
// durable review job created or reused by that selection.
type EligibleReviewScheduleResult struct {
	Matched       bool
	RuleID        string
	RuleVersionID string
	TriggerKind   string
	RunID         string
	JobID         string
	Created       bool
}

// EligibleReviewScheduler is the narrow command boundary for selecting the
// current active automatic rule. It has no GitHub capability.
type EligibleReviewScheduler interface {
	ScheduleEligibleReview(context.Context, string, string) (EligibleReviewScheduleResult, error)
}

// ReviewSchedulingOptions supplies selected-PR automatic review scheduling.
type ReviewSchedulingOptions struct {
	Scheduler EligibleReviewScheduler
}

// NewReviewSchedulingHandler exposes a deliberate local re-check of active
// automatic policy for one current canonical pull request.
func NewReviewSchedulingHandler(options ReviewSchedulingOptions) http.Handler {
	mux := http.NewServeMux()
	registerReviewSchedulingRoutes(mux, options)
	return mux
}

func registerReviewSchedulingRoutes(mux *http.ServeMux, options ReviewSchedulingOptions) {
	if mux == nil {
		return
	}
	handler := reviewSchedulingHandler{scheduler: options.Scheduler}
	mux.HandleFunc("POST /api/v1/mutate/pull-requests/{id}/schedule-review", handler.schedule)
}

type reviewSchedulingHandler struct {
	scheduler EligibleReviewScheduler
}

type reviewScheduleRequest struct {
	ConnectionID string `json:"connection_id"`
}

type reviewScheduleResponse struct {
	Matched       bool   `json:"matched"`
	RuleID        string `json:"rule_id,omitempty"`
	RuleVersionID string `json:"rule_version_id,omitempty"`
	TriggerKind   string `json:"trigger_kind,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	JobID         string `json:"job_id,omitempty"`
	Created       bool   `json:"created"`
}

func (h reviewSchedulingHandler) schedule(response http.ResponseWriter, request *http.Request) {
	pullRequestID := strings.TrimSpace(request.PathValue("id"))
	if pullRequestID == "" || len(pullRequestID) > 512 {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "pull request ID is invalid", false)
		return
	}
	if h.scheduler == nil {
		writeControlError(response, http.StatusServiceUnavailable, "review_execution_unavailable", "review execution is not enabled in this runtime", false)
		return
	}
	var input reviewScheduleRequest
	if err := decodeProposalMutationJSON(response, request, &input); err != nil {
		writeMutationDecodeError(response, err)
		return
	}
	if strings.TrimSpace(input.ConnectionID) == "" || len(input.ConnectionID) > 256 {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "connection ID is invalid", false)
		return
	}
	result, err := h.scheduler.ScheduleEligibleReview(request.Context(), input.ConnectionID, pullRequestID)
	if err != nil {
		if errors.Is(err, ErrReviewEvidenceNotReady) {
			writeControlError(response, http.StatusConflict, "review_evidence_not_ready", "current canonical evidence is required before review scheduling", false)
			return
		}
		writeControlError(response, http.StatusServiceUnavailable, "review_schedule_unavailable", "could not schedule eligible review", true)
		return
	}
	writeControlJSON(response, http.StatusOK, reviewScheduleResponse{
		Matched: result.Matched, RuleID: result.RuleID, RuleVersionID: result.RuleVersionID,
		TriggerKind: result.TriggerKind, RunID: result.RunID, JobID: result.JobID, Created: result.Created,
	})
}
