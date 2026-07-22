// Package publishworker records bounded, simulated publication outcomes from
// durable effect jobs. It has no GitHub write dependency.
package publishworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

// SimulateJobKind is durable job type for one approved publication effect.
const SimulateJobKind = "publication.simulate.v1"

const maxEffectIDBytes = 256

// SchedulerStore is the narrow durable scheduling boundary used by Scheduler.
type SchedulerStore interface {
	EnsureJob(context.Context, sqlite.JobInput) (sqlite.EnsureJobResult, error)
}

// Scheduler queues one simulated dispatch for an immutable publication effect.
// It does not inspect, authorize, or dispatch the effect itself.
type Scheduler struct {
	Store SchedulerStore
	Now   func() time.Time
}

// Schedule returns the matching active simulation job or creates one. The
// effect ID is deliberately the sole job payload: all outbound facts remain
// in the immutable effect record and are revalidated by Handler.
func (s Scheduler) Schedule(ctx context.Context, effectID string) (sqlite.EnsureJobResult, error) {
	if s.Store == nil {
		return sqlite.EnsureJobResult{}, errors.New("simulated publication job store is required")
	}
	if effectID == "" || effectID != strings.TrimSpace(effectID) || len(effectID) > maxEffectIDBytes || !validEffectID(effectID) {
		return sqlite.EnsureJobResult{}, errors.New("simulated publication effect ID is invalid")
	}
	payload, err := json.Marshal(jobPayload{EffectID: effectID})
	if err != nil {
		return sqlite.EnsureJobResult{}, fmt.Errorf("encode simulated publication job payload: %w", err)
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	result, err := s.Store.EnsureJob(ctx, sqlite.JobInput{
		Kind:         SimulateJobKind,
		ResourceType: "publication_effect",
		ResourceID:   effectID,
		DedupeKey:    SimulateJobKind + ":" + effectID,
		Payload:      payload,
		AvailableAt:  now,
		MaxAttempts:  3,
	})
	if err != nil {
		return sqlite.EnsureJobResult{}, fmt.Errorf("ensure simulated publication job: %w", err)
	}
	return result, nil
}

// EffectLoader loads an effect and its current-evidence status.
type EffectLoader interface {
	LoadCurrentPublicationEffect(context.Context, string) (sqlite.PublicationEffectTarget, error)
}

// AttemptRecorder idempotently records one bounded simulated attempt.
type AttemptRecorder interface {
	RecordSimulatedPublicationAttempt(context.Context, string, time.Time) (sqlite.RecordSimulatedPublicationAttemptResult, error)
}

// Handler records a simulated publication attempt only for a current effect
// explicitly authorized in simulated mode. Disabled effects complete without
// any dispatch. No implementation in this package can write to GitHub.
type Handler struct {
	Loader   EffectLoader
	Recorder AttemptRecorder
	Now      func() time.Time
}

// Handle implements worker.Handler.
func (h Handler) Handle(ctx context.Context, job sqlite.Job) error {
	if job.Kind != SimulateJobKind {
		return worker.Permanent(errors.New("unexpected simulated publication job kind"))
	}
	effectID, err := parseJobPayload(job.Payload)
	if err != nil {
		return worker.Permanent(fmt.Errorf("malformed simulated publication job payload: %w", err))
	}
	if h.Loader == nil || h.Recorder == nil {
		return worker.Permanent(errors.New("simulated publication handler dependencies are required"))
	}

	effect, err := h.Loader.LoadCurrentPublicationEffect(ctx, effectID)
	if err != nil {
		if errors.Is(err, sqlite.ErrPublicationEffectNotFound) || errors.Is(err, sqlite.ErrPublicationEffectNotCurrent) {
			return worker.Permanent(errors.New("simulated publication effect is no longer current"))
		}
		return errors.New("load simulated publication effect failed")
	}
	if effect.ID != effectID {
		return worker.Permanent(errors.New("simulated publication effect is no longer current"))
	}
	switch effect.PublicationMode {
	case sqlite.PublicationModeDisabled:
		return nil
	case sqlite.PublicationModeSimulated:
		// Continue below.
	default:
		return worker.Permanent(errors.New("simulated publication effect has unsupported mode"))
	}

	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}
	if _, err := h.Recorder.RecordSimulatedPublicationAttempt(ctx, effectID, now); err != nil {
		return errors.New("record simulated publication attempt failed")
	}
	return nil
}

type jobPayload struct {
	EffectID string `json:"effect_id"`
}

func parseJobPayload(raw []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload jobPayload
	if err := decoder.Decode(&payload); err != nil {
		return "", errors.New("must be a single supported JSON object")
	}
	if err := requireEOF(decoder); err != nil {
		return "", errors.New("must be a single JSON object")
	}
	if payload.EffectID == "" || payload.EffectID != strings.TrimSpace(payload.EffectID) ||
		len(payload.EffectID) > maxEffectIDBytes || !validEffectID(payload.EffectID) {
		return "", errors.New("effect ID is invalid")
	}
	return payload.EffectID, nil
}

func validEffectID(value string) bool {
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
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
