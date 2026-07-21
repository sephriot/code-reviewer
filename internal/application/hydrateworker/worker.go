// Package hydrateworker schedules and executes canonical GitHub hydration jobs.
package hydrateworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/hydrate"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

// HydrateJobKind is durable job type for one immutable GitHub observation.
const HydrateJobKind = "github.hydrate.v1"

const hydrateJobPayloadVersion = 1

// SchedulerStore is narrow durable boundary used by Scheduler.
type SchedulerStore interface {
	ListCanonicalHydrationTargets(context.Context, string) ([]sqlite.CanonicalHydrationTarget, error)
	EnsureJob(context.Context, sqlite.JobInput) (sqlite.EnsureJobResult, error)
}

// Scheduler queues one hydration job for each current active observation that
// lacks a canonical current revision.
type Scheduler struct {
	Store SchedulerStore
	Now   func() time.Time
}

// Schedule returns jobs created or reused for every eligible observation.
func (s Scheduler) Schedule(ctx context.Context, connectionID string) ([]sqlite.EnsureJobResult, error) {
	if s.Store == nil {
		return nil, errors.New("canonical hydration job store is required")
	}
	if strings.TrimSpace(connectionID) == "" {
		return nil, errors.New("canonical hydration connection ID is required")
	}
	targets, err := s.Store.ListCanonicalHydrationTargets(ctx, connectionID)
	if err != nil {
		return nil, fmt.Errorf("list canonical hydration targets: %w", err)
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	results := make([]sqlite.EnsureJobResult, 0, len(targets))
	for _, target := range targets {
		payload, err := marshalJobPayload(target)
		if err != nil {
			return nil, err
		}
		if target.ConnectionID != connectionID {
			return nil, errors.New("canonical hydration target belongs to another connection")
		}
		result, err := s.Store.EnsureJob(ctx, sqlite.JobInput{
			Kind:         HydrateJobKind,
			ResourceType: "pull_request_observation",
			ResourceID:   target.ObservationID,
			DedupeKey:    HydrateJobKind + ":" + target.ObservationID,
			Payload:      payload,
			AvailableAt:  now,
			MaxAttempts:  3,
		})
		if err != nil {
			return nil, fmt.Errorf("ensure canonical hydration job for observation %q: %w", target.ObservationID, err)
		}
		results = append(results, result)
	}
	return results, nil
}

// ReaderFactory resolves a connection reference to a read-only hydration
// reader. It must not grant publication capability.
type ReaderFactory func(context.Context, string) (hydrate.Reader, error)

// Handler executes one durable canonical hydration job.
type Handler struct {
	Store     hydrate.Store
	NewReader ReaderFactory
}

// Handle implements worker.Handler. Invalid job data is a permanent failure.
func (h Handler) Handle(ctx context.Context, job sqlite.Job) error {
	if job.Kind != HydrateJobKind {
		return worker.Permanent(errors.New("unexpected canonical hydration job kind"))
	}
	request, err := parseJobPayload(job.Payload)
	if err != nil {
		return worker.Permanent(fmt.Errorf("malformed hydration job payload: %w", err))
	}
	if h.Store == nil || h.NewReader == nil {
		return worker.Permanent(errors.New("canonical hydration handler dependencies are required"))
	}
	target, err := h.Store.FindCanonicalHydrationTarget(ctx, request.ConnectionID, request.Owner, request.Repository, request.Number)
	if err != nil {
		return fmt.Errorf("find scheduled canonical hydration target: %w", err)
	}
	if !matchesRequestTarget(request, target) {
		return worker.Permanent(errors.New("scheduled canonical hydration target is no longer current"))
	}
	reader, err := h.NewReader(ctx, request.ConnectionID)
	if err != nil {
		return fmt.Errorf("create GitHub canonical hydration reader: %w", err)
	}
	service := hydrate.Service{Reader: reader, Store: h.Store}
	if _, err := service.Hydrate(ctx, request); err != nil {
		return fmt.Errorf("hydrate GitHub canonical revision: %w", err)
	}
	return nil
}

func matchesRequestTarget(request hydrate.Request, target sqlite.CanonicalHydrationTarget) bool {
	return target.ConnectionID == request.ConnectionID && target.ObservationID == request.ObservationID &&
		strings.EqualFold(target.Owner, request.Owner) && strings.EqualFold(target.Repository, request.Repository) &&
		target.Number == request.Number
}

type hydrateJobPayload struct {
	Version       int    `json:"version"`
	ConnectionID  string `json:"connection_id"`
	ObservationID string `json:"observation_id"`
	Owner         string `json:"owner"`
	Repository    string `json:"repository"`
	Number        int    `json:"number"`
}

func marshalJobPayload(target sqlite.CanonicalHydrationTarget) ([]byte, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	return json.Marshal(hydrateJobPayload{
		Version: hydrateJobPayloadVersion, ConnectionID: target.ConnectionID, ObservationID: target.ObservationID,
		Owner: target.Owner, Repository: target.Repository, Number: target.Number,
	})
}

func parseJobPayload(raw []byte) (hydrate.Request, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload hydrateJobPayload
	if err := decoder.Decode(&payload); err != nil {
		return hydrate.Request{}, errors.New("must be a single supported JSON object")
	}
	if err := requireEOF(decoder); err != nil {
		return hydrate.Request{}, errors.New("must be a single JSON object")
	}
	if payload.Version != hydrateJobPayloadVersion {
		return hydrate.Request{}, fmt.Errorf("unsupported payload version %d", payload.Version)
	}
	request := hydrate.Request{
		ConnectionID: payload.ConnectionID, ObservationID: payload.ObservationID,
		Owner: payload.Owner, Repository: payload.Repository, Number: payload.Number,
	}
	if err := validateRequest(request); err != nil {
		return hydrate.Request{}, err
	}
	return request, nil
}

func validateTarget(target sqlite.CanonicalHydrationTarget) error {
	return validateRequest(hydrate.Request{
		ConnectionID: target.ConnectionID, ObservationID: target.ObservationID,
		Owner: target.Owner, Repository: target.Repository, Number: target.Number,
	})
}

func validateRequest(request hydrate.Request) error {
	if strings.TrimSpace(request.ConnectionID) == "" || strings.TrimSpace(request.ObservationID) == "" ||
		strings.TrimSpace(request.Owner) == "" || strings.TrimSpace(request.Repository) == "" ||
		strings.Contains(request.Owner, "/") || strings.Contains(request.Repository, "/") || request.Number <= 0 {
		return errors.New("canonical hydration target identity is required")
	}
	return nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("extra value")
	}
	return err
}

var _ worker.Handler = Handler{}
