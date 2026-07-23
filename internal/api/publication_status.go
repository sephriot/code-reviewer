package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

const publicationEffectStatusPath = "/api/v1/publication-effects/{id}"

// PublicationEffectStatusReader exposes safe, local delivery state only.
type PublicationEffectStatusReader interface {
	PublicationEffectStatus(context.Context, string) (sqlite.PublicationEffectStatus, error)
}

// PublicationEffectStatusOptions supplies immutable status reads.
type PublicationEffectStatusOptions struct {
	Reader PublicationEffectStatusReader
}

func registerPublicationEffectStatusRoutes(mux *http.ServeMux, options PublicationEffectStatusOptions) {
	if mux == nil {
		return
	}
	handler := publicationEffectStatusHandler{reader: options.Reader}
	mux.HandleFunc("GET "+publicationEffectStatusPath, handler.get)
}

type publicationEffectStatusHandler struct {
	reader PublicationEffectStatusReader
}

func (h publicationEffectStatusHandler) get(response http.ResponseWriter, request *http.Request) {
	if !isLoopbackRemoteAddress(request.RemoteAddr) {
		writeControlError(response, http.StatusForbidden, "loopback_required", "publication effect status is available only on loopback", false)
		return
	}
	if request.URL.RawQuery != "" {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "publication effect status does not accept query parameters", false)
		return
	}
	effectID, ok := validPublicationEffectPathID(request)
	if !ok {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "publication effect ID is invalid", false)
		return
	}
	if h.reader == nil {
		writeControlError(response, http.StatusServiceUnavailable, "publication_status_unavailable", "publication effect status is unavailable", true)
		return
	}
	status, err := h.reader.PublicationEffectStatus(request.Context(), effectID)
	if errors.Is(err, sqlite.ErrPublicationEffectNotFound) {
		writeControlError(response, http.StatusNotFound, "publication_effect_not_found", "publication effect was not found", false)
		return
	}
	if err != nil {
		writeControlError(response, http.StatusServiceUnavailable, "publication_status_unavailable", "publication effect status is unavailable", true)
		return
	}
	result := map[string]any{
		"effect_id": status.EffectID, "publication_mode": string(status.PublicationMode),
	}
	if status.Attempt != nil {
		result["attempt"] = map[string]string{
			"id": status.Attempt.AttemptID, "publication_mode": string(status.Attempt.PublicationMode),
			"outcome": status.Attempt.Outcome, "completed_at": status.Attempt.CompletedAt.Format(time.RFC3339Nano),
		}
	}
	if status.Resolution != nil {
		result["resolution"] = map[string]string{
			"id": status.Resolution.ResolutionID, "resolution": string(status.Resolution.Resolution),
			"resolved_at": status.Resolution.ResolvedAt.Format(time.RFC3339Nano),
		}
	}
	writeControlJSON(response, http.StatusOK, result)
}
