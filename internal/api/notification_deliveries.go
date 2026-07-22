package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

const browserNotificationDeliveriesPath = "/api/v1/notification-deliveries/browser"

// BrowserNotificationDeliveryStore provides only loopback dashboard browser
// delivery reads and terminal acknowledgements.
type BrowserNotificationDeliveryStore interface {
	ListQueuedBrowserNotificationDeliveries(context.Context, int) ([]sqlite.NotificationDeliveryTarget, error)
	RecordNotificationDeliveryOutcome(context.Context, sqlite.NotificationDeliveryOutcome) (sqlite.RecordNotificationDeliveryOutcomeResult, error)
}

// BrowserNotificationDeliveryOptions supplies one local-only browser delivery
// store. Mutations remain protected by MutationAuth.Wrap.
type BrowserNotificationDeliveryOptions struct {
	Store BrowserNotificationDeliveryStore
	Now   func() time.Time
}

func registerBrowserNotificationDeliveryRoutes(mux *http.ServeMux, options BrowserNotificationDeliveryOptions) {
	if mux == nil {
		return
	}
	handler := browserNotificationDeliveryHandler{store: options.Store, now: options.Now}
	mux.HandleFunc("GET "+browserNotificationDeliveriesPath, handler.list)
	mux.HandleFunc("POST /api/v1/mutate/notification-deliveries/browser/{id}/outcome", handler.recordOutcome)
}

type browserNotificationDeliveryHandler struct {
	store BrowserNotificationDeliveryStore
	now   func() time.Time
}

type browserNotificationDeliveryResponse struct {
	ID        string `json:"id"`
	EventType string `json:"event_type"`
}

type browserNotificationDeliveryPageResponse struct {
	Items []browserNotificationDeliveryResponse `json:"items"`
}

type browserNotificationOutcomeRequest struct {
	State string `json:"state"`
}

func (h browserNotificationDeliveryHandler) list(response http.ResponseWriter, request *http.Request) {
	if !isLoopbackRemoteAddress(request.RemoteAddr) {
		writeControlError(response, http.StatusForbidden, "loopback_required", "browser notifications are available only on loopback", false)
		return
	}
	if h.store == nil {
		writeControlError(response, http.StatusServiceUnavailable, "notification_deliveries_unavailable", "browser notifications are unavailable", true)
		return
	}
	items, err := h.store.ListQueuedBrowserNotificationDeliveries(request.Context(), 50)
	if err != nil {
		writeControlError(response, http.StatusServiceUnavailable, "notification_deliveries_unavailable", "browser notifications are unavailable", true)
		return
	}
	result := browserNotificationDeliveryPageResponse{Items: make([]browserNotificationDeliveryResponse, 0, len(items))}
	for _, item := range items {
		result.Items = append(result.Items, browserNotificationDeliveryResponse{ID: item.ID, EventType: item.EventType})
	}
	writeControlJSON(response, http.StatusOK, result)
}

func (h browserNotificationDeliveryHandler) recordOutcome(response http.ResponseWriter, request *http.Request) {
	if h.store == nil {
		writeControlError(response, http.StatusServiceUnavailable, "notification_deliveries_unavailable", "browser notifications are unavailable", true)
		return
	}
	id, ok := validBrowserNotificationDeliveryID(request)
	if !ok {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "browser notification delivery ID is invalid", false)
		return
	}
	var input browserNotificationOutcomeRequest
	if err := decodeProposalMutationJSON(response, request, &input); err != nil {
		writeMutationDecodeError(response, err)
		return
	}
	if input.State != string(sqlite.NotificationDeliveryDelivered) && input.State != string(sqlite.NotificationDeliverySuppressed) {
		writeControlError(response, http.StatusBadRequest, "invalid_request", "browser notification outcome is invalid", false)
		return
	}
	now := time.Now().UTC()
	if h.now != nil {
		now = h.now().UTC()
	}
	_, err := h.store.RecordNotificationDeliveryOutcome(request.Context(), sqlite.NotificationDeliveryOutcome{ID: id, State: sqlite.NotificationDeliveryState(input.State), AttemptedAt: now})
	if err != nil {
		if errors.Is(err, sqlite.ErrNotificationDeliveryNotFound) {
			writeControlError(response, http.StatusNotFound, "notification_delivery_not_found", "browser notification delivery was not found", false)
			return
		}
		if errors.Is(err, sqlite.ErrNotificationDeliveryNotPending) {
			writeControlError(response, http.StatusConflict, "notification_delivery_not_pending", "browser notification delivery is no longer pending", false)
			return
		}
		writeControlError(response, http.StatusServiceUnavailable, "notification_deliveries_unavailable", "browser notifications are unavailable", true)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func validBrowserNotificationDeliveryID(request *http.Request) (string, bool) {
	value := strings.TrimSpace(request.PathValue("id"))
	if value == "" || value != request.PathValue("id") || len(value) > 256 {
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
