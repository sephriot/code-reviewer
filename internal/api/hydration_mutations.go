package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ErrHydrationTargetNotFound lets the runtime expose a stale or unavailable
// selected observation without revealing persistence details.
var ErrHydrationTargetNotFound = errors.New("canonical hydration target not found")

// HydrationScheduleResult names a durable hydration job created or reused by
// a local dashboard command.
type HydrationScheduleResult struct {
	JobID   string
	Created bool
}

// PullRequestHydrationScheduler is the narrow local command boundary. It has
// no GitHub write capability.
type PullRequestHydrationScheduler interface {
	SchedulePullRequest(context.Context, string, string) (HydrationScheduleResult, error)
}

// HydrationMutationOptions supplies the selected-observation scheduler.
type HydrationMutationOptions struct {
	Scheduler PullRequestHydrationScheduler
}

// NewHydrationMutationHandler exposes a local request to build canonical
// evidence for one current observed pull request.
func NewHydrationMutationHandler(options HydrationMutationOptions) http.Handler {
	mux := http.NewServeMux()
	registerHydrationMutationRoutes(mux, options)
	return mux
}

func registerHydrationMutationRoutes(mux *http.ServeMux, options HydrationMutationOptions) {
	if mux == nil {
		return
	}
	handler := hydrationMutationHandler{scheduler: options.Scheduler}
	mux.HandleFunc("POST /api/v1/mutate/pull-requests/{id}/hydrate", handler.schedule)
}

type hydrationMutationHandler struct {
	scheduler PullRequestHydrationScheduler
}

type hydrationScheduleRequest struct {
	ConnectionID string `json:"connection_id"`
}

type hydrationScheduleResponse struct {
	JobID   string `json:"job_id"`
	Created bool   `json:"created"`
}

func (h hydrationMutationHandler) schedule(response http.ResponseWriter, request *http.Request) {
	pullRequestID := strings.TrimSpace(request.PathValue("id"))
	if pullRequestID == "" || len(pullRequestID) > 512 {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "pull request ID is invalid", false)
		return
	}
	if h.scheduler == nil {
		writeControlError(response, http.StatusServiceUnavailable, "mutation_unavailable", "canonical hydration is unavailable", true)
		return
	}
	var input hydrationScheduleRequest
	if err := decodeProposalMutationJSON(response, request, &input); err != nil {
		writeMutationDecodeError(response, err)
		return
	}
	if strings.TrimSpace(input.ConnectionID) == "" || len(input.ConnectionID) > 256 {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "connection ID is invalid", false)
		return
	}
	result, err := h.scheduler.SchedulePullRequest(request.Context(), input.ConnectionID, pullRequestID)
	if err != nil {
		switch {
		case errors.Is(err, ErrHydrationTargetNotFound):
			writeControlError(response, http.StatusNotFound, "hydration_target_not_found", "current pull request evidence is unavailable", false)
		default:
			writeControlError(response, http.StatusServiceUnavailable, "hydration_unavailable", "could not schedule canonical hydration", true)
		}
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeControlJSON(response, status, hydrationScheduleResponse{JobID: result.JobID, Created: result.Created})
}
