package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

// PublicationEffectCreator is the only persistence capability needed to
// authorize one durable effect from an already-approved proposal revision.
type PublicationEffectCreator interface {
	CreatePublicationEffect(context.Context, sqlite.CreatePublicationEffectInput) (sqlite.CreatePublicationEffectResult, error)
}

// SimulatedPublicationScheduler queues one local, effect-bound simulation
// job. It has no GitHub publication capability.
type SimulatedPublicationScheduler interface {
	Schedule(context.Context, string) (sqlite.EnsureJobResult, error)
}

// PublicationMutationOptions supplies the narrow capabilities for local
// simulated-publication requests. A nil Scheduler fails closed when a
// database has simulated mode but this runtime has not enabled its worker.
type PublicationMutationOptions struct {
	Effects   PublicationEffectCreator
	Scheduler SimulatedPublicationScheduler
	Now       func() time.Time
}

// NewPublicationMutationHandler exposes local simulated-publication requests.
// Callers must mount it under MutationAuth.Wrap.
func NewPublicationMutationHandler(options PublicationMutationOptions) http.Handler {
	mux := http.NewServeMux()
	handler := publicationMutationHandler{effects: options.Effects, scheduler: options.Scheduler, now: options.Now}
	mux.HandleFunc("POST /api/v1/mutate/proposal-revisions/{id}/publication/simulate", handler.simulate)
	return mux
}

func registerPublicationMutationRoutes(mux *http.ServeMux, options PublicationMutationOptions) {
	if mux == nil {
		return
	}
	handler := publicationMutationHandler{effects: options.Effects, scheduler: options.Scheduler, now: options.Now}
	mux.HandleFunc("POST /api/v1/mutate/proposal-revisions/{id}/publication/simulate", handler.simulate)
}

type publicationMutationHandler struct {
	effects   PublicationEffectCreator
	scheduler SimulatedPublicationScheduler
	now       func() time.Time
}

type simulatePublicationRequest struct {
	// IdempotencyKey is optional. When omitted, the store derives a stable key
	// from the approved immutable proposal revision and outbound payload.
	IdempotencyKey string `json:"idempotency_key"`
}

type simulatePublicationResponse struct {
	EffectID        string `json:"effect_id"`
	PublicationMode string `json:"publication_mode"`
	Created         bool   `json:"created"`
	Job             *struct {
		ID      string `json:"id"`
		Created bool   `json:"created"`
	} `json:"job"`
}

func (h publicationMutationHandler) simulate(response http.ResponseWriter, request *http.Request) {
	proposalRevisionID, ok := validProposalRevisionPathID(request)
	if !ok {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "proposal revision ID is invalid", false)
		return
	}
	if h.effects == nil {
		writeControlError(response, http.StatusServiceUnavailable, "mutation_unavailable", "publication mutation service is unavailable", true)
		return
	}
	var input simulatePublicationRequest
	if err := decodeProposalMutationJSON(response, request, &input); err != nil {
		writeMutationDecodeError(response, err)
		return
	}
	if !validPublicationIdempotencyKey(input.IdempotencyKey) {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "idempotency key is invalid", false)
		return
	}
	effect, err := h.effects.CreatePublicationEffect(request.Context(), sqlite.CreatePublicationEffectInput{
		ProposalRevisionID: proposalRevisionID,
		IdempotencyKey:     input.IdempotencyKey,
		CreatedAt:          h.currentTime(),
	})
	if err != nil {
		writePublicationMutationStoreError(response, err)
		return
	}
	result := simulatePublicationResponse{
		EffectID:        effect.EffectID,
		PublicationMode: string(effect.PublicationMode),
		Created:         effect.Created,
	}
	if effect.PublicationMode == sqlite.PublicationModeSimulated {
		if h.scheduler == nil {
			writeControlError(response, http.StatusServiceUnavailable, "simulation_unavailable", "simulated publication worker is unavailable", true)
			return
		}
		job, err := h.scheduler.Schedule(request.Context(), effect.EffectID)
		if err != nil {
			writeControlError(response, http.StatusServiceUnavailable, "simulation_schedule_failed", "could not schedule simulated publication", true)
			return
		}
		result.Job = &struct {
			ID      string `json:"id"`
			Created bool   `json:"created"`
		}{ID: job.ID, Created: job.Created}
	}
	status := http.StatusCreated
	if !effect.Created {
		status = http.StatusOK
	}
	writeControlJSON(response, status, result)
}

func validProposalRevisionPathID(request *http.Request) (string, bool) {
	value := strings.TrimSpace(request.PathValue("id"))
	if value == "" || value != request.PathValue("id") || len(value) > 512 {
		return "", false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return "", false
	}
	return value, true
}

func validPublicationIdempotencyKey(value string) bool {
	if value == "" {
		return true
	}
	if value != strings.TrimSpace(value) || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' || character == ':' {
			continue
		}
		return false
	}
	return true
}

func (h publicationMutationHandler) currentTime() time.Time {
	if h.now != nil {
		return h.now().UTC()
	}
	return time.Now().UTC()
}

func writePublicationMutationStoreError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sqlite.ErrPublicationAuthorizationNotFound):
		writeControlError(response, http.StatusConflict, "publication_not_authorized", "proposal revision is not approved for publication", false)
	case errors.Is(err, sqlite.ErrPublicationEffectConflict):
		writeControlError(response, http.StatusConflict, "publication_conflict", "publication effect conflicts with existing immutable facts", false)
	case errors.Is(err, sqlite.ErrPublicationModeUnsupported):
		writeControlError(response, http.StatusConflict, "publication_mode_unsupported", "publication mode is not supported", false)
	case errors.Is(err, sqlite.ErrCanonicalReviewTargetNotFound), strings.Contains(err.Error(), "no longer matches current canonical evidence"):
		writeControlError(response, http.StatusConflict, "proposal_not_current", "proposal no longer matches current canonical evidence", false)
	default:
		writeControlError(response, http.StatusInternalServerError, "mutation_failed", "could not create publication effect", true)
	}
}
