package publishworker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

func TestHandlerRecordsCurrentSimulatedEffect(t *testing.T) {
	loader := &loaderRecorder{effect: Effect{ID: "effect-1", PublicationMode: sqlite.PublicationModeSimulated, EvidenceCurrent: true}}
	recorder := &attemptRecorder{}
	handler := Handler{Loader: loader, Recorder: recorder, Now: func() time.Time { return time.Unix(7, 0).UTC() }}

	if err := handler.Handle(context.Background(), publicationJob(`{"effect_id":"effect-1"}`)); err != nil {
		t.Fatal(err)
	}
	if loader.effectID != "effect-1" || recorder.effectID != "effect-1" || !recorder.at.Equal(time.Unix(7, 0).UTC()) {
		t.Fatalf("loader=%+v recorder=%+v", loader, recorder)
	}
}

func TestHandlerCompletesDisabledEffectWithoutDispatch(t *testing.T) {
	loader := &loaderRecorder{effect: Effect{ID: "effect-1", PublicationMode: sqlite.PublicationModeDisabled, EvidenceCurrent: true}}
	recorder := &attemptRecorder{}
	handler := Handler{Loader: loader, Recorder: recorder}

	if err := handler.Handle(context.Background(), publicationJob(`{"effect_id":"effect-1"}`)); err != nil {
		t.Fatal(err)
	}
	if recorder.effectID != "" {
		t.Fatalf("disabled effect was dispatched: %+v", recorder)
	}
}

func TestHandlerRejectsMalformedPayloadBeforeLoading(t *testing.T) {
	loader := &loaderRecorder{}
	handler := Handler{Loader: loader, Recorder: &attemptRecorder{}}
	for _, payload := range []string{
		`{}`, `{"effect_id":"effect-1","extra":true}`, `{"effect_id":" effect-1"}`,
		`{"effect_id":"effect/1"}`, `{"effect_id":"effect-1"} null`,
	} {
		err := handler.Handle(context.Background(), publicationJob(payload))
		if err == nil || !worker.IsPermanent(err) || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("payload=%s err=%v", payload, err)
		}
	}
	if loader.effectID != "" {
		t.Fatalf("loader called for malformed payload: %+v", loader)
	}
}

func TestHandlerMarksStaleEffectTerminal(t *testing.T) {
	tests := []struct {
		name   string
		effect Effect
		err    error
	}{
		{name: "missing", err: ErrEffectNotFound},
		{name: "stale error", err: ErrEffectEvidenceStale},
		{name: "stale state", effect: Effect{ID: "effect-1", PublicationMode: sqlite.PublicationModeSimulated}},
		{name: "wrong ID", effect: Effect{ID: "effect-2", PublicationMode: sqlite.PublicationModeSimulated, EvidenceCurrent: true}},
		{name: "enabled", effect: Effect{ID: "effect-1", PublicationMode: "enabled", EvidenceCurrent: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &attemptRecorder{}
			handler := Handler{Loader: &loaderRecorder{effect: test.effect, err: test.err}, Recorder: recorder}
			err := handler.Handle(context.Background(), publicationJob(`{"effect_id":"effect-1"}`))
			if err == nil || !worker.IsPermanent(err) || recorder.effectID != "" {
				t.Fatalf("err=%v recorder=%+v", err, recorder)
			}
		})
	}
}

func TestHandlerRetriesStorageFailures(t *testing.T) {
	tests := []struct {
		name     string
		loader   *loaderRecorder
		recorder *attemptRecorder
	}{
		{name: "load", loader: &loaderRecorder{err: errors.New("database offline")}, recorder: &attemptRecorder{}},
		{name: "record", loader: &loaderRecorder{effect: Effect{ID: "effect-1", PublicationMode: sqlite.PublicationModeSimulated, EvidenceCurrent: true}}, recorder: &attemptRecorder{err: errors.New("database offline")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (Handler{Loader: test.loader, Recorder: test.recorder}).Handle(context.Background(), publicationJob(`{"effect_id":"effect-1"}`))
			if err == nil || worker.IsPermanent(err) || strings.Contains(err.Error(), "offline") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestHandlerRejectsInvalidDependenciesAndJobKind(t *testing.T) {
	if err := (Handler{}).Handle(context.Background(), publicationJob(`{"effect_id":"effect-1"}`)); err == nil || !worker.IsPermanent(err) {
		t.Fatalf("dependencies err=%v", err)
	}
	job := publicationJob(`{"effect_id":"effect-1"}`)
	job.Kind = "other"
	if err := (Handler{}).Handle(context.Background(), job); err == nil || !worker.IsPermanent(err) {
		t.Fatalf("kind err=%v", err)
	}
}

func publicationJob(payload string) sqlite.Job {
	return sqlite.Job{Kind: SimulateJobKind, Payload: []byte(payload)}
}

type loaderRecorder struct {
	effectID string
	effect   Effect
	err      error
}

func (r *loaderRecorder) LoadPublicationEffect(_ context.Context, effectID string) (Effect, error) {
	r.effectID = effectID
	return r.effect, r.err
}

type attemptRecorder struct {
	effectID string
	at       time.Time
	err      error
}

func (r *attemptRecorder) RecordSimulatedPublicationAttempt(_ context.Context, effectID string, at time.Time) error {
	r.effectID, r.at = effectID, at
	return r.err
}
