// Package notificationoutbox translates one durable event-outbox topic into
// local notification delivery work.
package notificationoutbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/sephriot/code-reviewer/internal/application/notificationdispatch"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

// DispatchTopic is the only outbox topic this handler accepts.
const DispatchTopic = "notification.dispatch.v1"

// Dispatcher derives and schedules local delivery work for one committed event.
type Dispatcher interface {
	Dispatch(context.Context, notificationdispatch.Request) (notificationdispatch.Result, error)
}

// Handler accepts only a strict local notification dispatch envelope.
type Handler struct{ Dispatcher Dispatcher }

// Handle implements outbox.Handler. Invalid envelopes are permanent; storage
// failures remain retryable under the fenced outbox lease.
func (h Handler) Handle(ctx context.Context, delivery sqlite.OutboxDelivery) error {
	if delivery.Topic != DispatchTopic {
		return worker.Permanent(errors.New("unexpected notification outbox topic"))
	}
	if h.Dispatcher == nil {
		return worker.Permanent(errors.New("notification dispatcher is required"))
	}
	request, err := parsePayload(delivery.Payload)
	if err != nil {
		return worker.Permanent(fmt.Errorf("malformed notification dispatch payload: %w", err))
	}
	if _, err := h.Dispatcher.Dispatch(ctx, request); err != nil {
		return errors.New("notification dispatch failed")
	}
	return nil
}

type payload struct {
	DomainEventID string          `json:"domain_event_id"`
	DedupeKey     string          `json:"dedupe_key"`
	Payload       json.RawMessage `json:"payload"`
}

func parsePayload(raw []byte) (notificationdispatch.Request, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var decoded payload
	if err := decoder.Decode(&decoded); err != nil {
		return notificationdispatch.Request{}, errors.New("must be a single supported JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return notificationdispatch.Request{}, errors.New("must be a single supported JSON object")
	}
	if decoded.DomainEventID == "" || decoded.DedupeKey == "" || len(decoded.DomainEventID) > 512 || len(decoded.DedupeKey) > 512 {
		return notificationdispatch.Request{}, errors.New("event identity is invalid")
	}
	var object map[string]any
	if err := json.Unmarshal(decoded.Payload, &object); err != nil || object == nil {
		return notificationdispatch.Request{}, errors.New("payload must be an object")
	}
	return notificationdispatch.Request{DomainEventID: decoded.DomainEventID, DedupeKey: decoded.DedupeKey, PayloadJSON: decoded.Payload}, nil
}
