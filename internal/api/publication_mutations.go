package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/publishworker"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

// AtomicPublicationEffectCreator creates or reloads one durable effect and
// its matching worker job in the same transaction.
type AtomicPublicationEffectCreator interface {
	CreatePublicationEffectAndEnsureJob(context.Context, sqlite.CreatePublicationEffectInput, sqlite.PublicationEffectJobFactory) (sqlite.CreatePublicationEffectResult, error)
}

// PublicationUncertaintyResolver records a human terminal classification of
// one uncertain enabled delivery. It has no scheduling or GitHub capability.
type PublicationUncertaintyResolver interface {
	ResolvePublicationUncertainty(context.Context, sqlite.ResolvePublicationUncertaintyInput) (sqlite.ResolvePublicationUncertaintyResult, error)
}

// PublicationMutationOptions supplies narrow, atomic publication
// capabilities. A missing atomic creator fails closed.
type PublicationMutationOptions struct {
	AtomicEffects       AtomicPublicationEffectCreator
	UncertaintyResolver PublicationUncertaintyResolver
	Now                 func() time.Time
}

// NewPublicationMutationHandler exposes guarded publication requests. Callers
// must mount it under MutationAuth.Wrap.
func NewPublicationMutationHandler(options PublicationMutationOptions) http.Handler {
	mux := http.NewServeMux()
	handler := publicationMutationHandler{atomicEffects: options.AtomicEffects, uncertaintyResolver: options.UncertaintyResolver, now: options.Now}
	mux.HandleFunc("POST /api/v1/mutate/proposal-revisions/{id}/publication/simulate", handler.simulate)
	mux.HandleFunc("POST /api/v1/mutate/proposal-revisions/{id}/publication/dispatch", handler.dispatch)
	mux.HandleFunc("POST /api/v1/mutate/publication-effects/{id}/uncertainty-resolution", handler.resolveUncertainty)
	return mux
}

func registerPublicationMutationRoutes(mux *http.ServeMux, options PublicationMutationOptions) {
	if mux == nil {
		return
	}
	handler := publicationMutationHandler{atomicEffects: options.AtomicEffects, uncertaintyResolver: options.UncertaintyResolver, now: options.Now}
	mux.HandleFunc("POST /api/v1/mutate/proposal-revisions/{id}/publication/simulate", handler.simulate)
	mux.HandleFunc("POST /api/v1/mutate/proposal-revisions/{id}/publication/dispatch", handler.dispatch)
	mux.HandleFunc("POST /api/v1/mutate/publication-effects/{id}/uncertainty-resolution", handler.resolveUncertainty)
}

type publicationMutationHandler struct {
	atomicEffects       AtomicPublicationEffectCreator
	uncertaintyResolver PublicationUncertaintyResolver
	now                 func() time.Time
}

type simulatePublicationRequest struct {
	// IdempotencyKey is optional. When omitted, the store derives a stable key
	// from the approved immutable proposal revision and outbound payload.
	IdempotencyKey string `json:"idempotency_key"`
}

type publicationResponse struct {
	EffectID        string `json:"effect_id"`
	PublicationMode string `json:"publication_mode"`
	Created         bool   `json:"created"`
	Job             *struct {
		ID      string `json:"id"`
		Created bool   `json:"created"`
	} `json:"job"`
}

type publicationUncertaintyResolutionRequest struct {
	Resolution     string `json:"resolution"`
	ActorID        string `json:"actor_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Reason         string `json:"reason"`
}

type publicationUncertaintyResolutionResponse struct {
	ResolutionID string `json:"resolution_id"`
	Created      bool   `json:"created"`
}

func (h publicationMutationHandler) simulate(response http.ResponseWriter, request *http.Request) {
	h.atomic(response, request, []sqlite.PublicationMode{sqlite.PublicationModeDisabled, sqlite.PublicationModeSimulated})
}

// dispatch is an explicit human request for one enabled external publication.
// It never schedules disabled or simulated effects.
func (h publicationMutationHandler) dispatch(response http.ResponseWriter, request *http.Request) {
	h.atomic(response, request, []sqlite.PublicationMode{sqlite.PublicationModeEnabled})
}

func (h publicationMutationHandler) atomic(response http.ResponseWriter, request *http.Request, modes []sqlite.PublicationMode) {
	proposalRevisionID, ok := validProposalRevisionPathID(request)
	if !ok {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "proposal revision ID is invalid", false)
		return
	}
	var body simulatePublicationRequest
	if err := decodeProposalMutationJSON(response, request, &body); err != nil {
		writeMutationDecodeError(response, err)
		return
	}
	if !validPublicationIdempotencyKey(body.IdempotencyKey) {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "idempotency key is invalid", false)
		return
	}
	if h.atomicEffects == nil {
		writeControlError(response, http.StatusServiceUnavailable, "mutation_unavailable", "atomic publication mutation service is unavailable", true)
		return
	}
	effect, err := h.atomicEffects.CreatePublicationEffectAndEnsureJob(request.Context(), sqlite.CreatePublicationEffectInput{ProposalRevisionID: proposalRevisionID, IdempotencyKey: body.IdempotencyKey, AllowedModes: modes, CreatedAt: h.currentTime()}, atomicPublicationJob)
	if err != nil {
		writePublicationMutationStoreError(response, err)
		return
	}
	result := publicationResponse{EffectID: effect.EffectID, PublicationMode: string(effect.PublicationMode), Created: effect.Created, Job: publicationJobResponseValue(effect.Job)}
	writePublicationResponse(response, effect.Created, result)
}
func publicationJobResponseValue(job *sqlite.EnsureJobResult) *struct {
	ID      string `json:"id"`
	Created bool   `json:"created"`
} {
	if job == nil {
		return nil
	}
	return &struct {
		ID      string `json:"id"`
		Created bool   `json:"created"`
	}{ID: job.ID, Created: job.Created}
}
func atomicPublicationJob(effectID string, mode sqlite.PublicationMode) (sqlite.JobInput, error) {
	payload, err := json.Marshal(struct {
		EffectID string `json:"effect_id"`
	}{effectID})
	if err != nil {
		return sqlite.JobInput{}, err
	}
	if mode == sqlite.PublicationModeSimulated {
		return sqlite.JobInput{Kind: publishworker.SimulateJobKind, ResourceType: "publication_effect", ResourceID: effectID, DedupeKey: publishworker.SimulateJobKind + ":" + effectID, Payload: payload, MaxAttempts: 3}, nil
	}
	if mode == sqlite.PublicationModeEnabled {
		return sqlite.JobInput{Kind: publishworker.EnabledJobKind, ResourceType: "publication_effect", ResourceID: effectID, DedupeKey: publishworker.EnabledJobKind + ":" + effectID, Payload: payload, MaxAttempts: 1}, nil
	}
	return sqlite.JobInput{}, errors.New("publication job mode unsupported")
}

// resolveUncertainty records an explicit human terminal resolution. It never
// sends, schedules, or retries an external publication.
func (h publicationMutationHandler) resolveUncertainty(response http.ResponseWriter, request *http.Request) {
	effectID, ok := validPublicationEffectPathID(request)
	if !ok {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "publication effect ID is invalid", false)
		return
	}
	if h.uncertaintyResolver == nil {
		writeControlError(response, http.StatusServiceUnavailable, "mutation_unavailable", "publication uncertainty resolution service is unavailable", true)
		return
	}
	var input publicationUncertaintyResolutionRequest
	if err := decodeProposalMutationJSON(response, request, &input); err != nil {
		writeMutationDecodeError(response, err)
		return
	}
	if strings.TrimSpace(input.ActorID) == "" || input.IdempotencyKey == "" || !validPublicationIdempotencyKey(input.IdempotencyKey) ||
		(input.Resolution != string(sqlite.PublicationUncertaintyExternallyCompleted) && input.Resolution != string(sqlite.PublicationUncertaintyAbandoned)) {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "publication uncertainty resolution fields are invalid", false)
		return
	}
	resolved, err := h.uncertaintyResolver.ResolvePublicationUncertainty(request.Context(), sqlite.ResolvePublicationUncertaintyInput{
		EffectID: effectID, Resolution: sqlite.PublicationUncertaintyResolution(input.Resolution), ActorID: input.ActorID,
		IdempotencyKey: input.IdempotencyKey, Reason: input.Reason, ResolvedAt: h.currentTime(),
	})
	if err != nil {
		writePublicationUncertaintyResolutionStoreError(response, err)
		return
	}
	status := http.StatusOK
	if resolved.Created {
		status = http.StatusCreated
	}
	writeControlJSON(response, status, publicationUncertaintyResolutionResponse{ResolutionID: resolved.ResolutionID, Created: resolved.Created})
}

func writePublicationResponse(response http.ResponseWriter, created bool, result publicationResponse) {
	status := http.StatusCreated
	if !created {
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

func validPublicationEffectPathID(request *http.Request) (string, bool) {
	value := strings.TrimSpace(request.PathValue("id"))
	if value == "" || value != request.PathValue("id") || len(value) > 512 {
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
	case errors.Is(err, sqlite.ErrPublicationModeNotAllowed):
		writeControlError(response, http.StatusConflict, "enabled_publication_required", "GitHub publication requires enabled publication mode", false)
	case errors.Is(err, sqlite.ErrCanonicalReviewTargetNotFound), strings.Contains(err.Error(), "no longer matches current canonical evidence"):
		writeControlError(response, http.StatusConflict, "proposal_not_current", "proposal no longer matches current canonical evidence", false)
	default:
		writeControlError(response, http.StatusInternalServerError, "mutation_failed", "could not create publication effect", true)
	}
}

func writePublicationUncertaintyResolutionStoreError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sqlite.ErrPublicationEffectNotFound):
		writeControlError(response, http.StatusNotFound, "publication_effect_not_found", "publication effect was not found", false)
	case errors.Is(err, sqlite.ErrPublicationUncertaintyNotResolvable):
		writeControlError(response, http.StatusConflict, "publication_not_uncertain", "publication effect has no unresolved uncertain delivery", false)
	case errors.Is(err, sqlite.ErrPublicationUncertaintyResolutionConflict):
		writeControlError(response, http.StatusConflict, "publication_resolution_conflict", "publication uncertainty resolution conflicts with immutable facts", false)
	default:
		writeControlError(response, http.StatusInternalServerError, "mutation_failed", "could not resolve publication uncertainty", true)
	}
}
