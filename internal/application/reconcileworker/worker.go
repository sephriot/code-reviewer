// Package reconcileworker schedules and executes shadow GitHub reconciliation jobs.
package reconcileworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	githubadapter "github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/application/reconcile"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
	"github.com/sephriot/code-reviewer/internal/worker"
)

// ReconcileJobKind is durable job type for one shadow GitHub reconciliation.
const ReconcileJobKind = "github.reconcile.v1"

const reconcileJobPayloadVersion = 1

// JobStore is narrow durable scheduling boundary used by Scheduler.
type JobStore interface {
	EnsureJob(context.Context, sqlite.JobInput) (sqlite.EnsureJobResult, error)
}

// Scheduler ensures one active shadow reconciliation job per GitHub connection.
// Its payload names credential reference but never carries credential material.
type Scheduler struct {
	Store JobStore
	Now   func() time.Time
}

// Schedule returns active reconcile job for config's connection, creating it
// only when no queued, running, or retrying equivalent job exists.
func (s Scheduler) Schedule(ctx context.Context, config reconcile.Config) (sqlite.EnsureJobResult, error) {
	if s.Store == nil {
		return sqlite.EnsureJobResult{}, errors.New("reconciliation job store is required")
	}
	payload, err := marshalJobPayload(config)
	if err != nil {
		return sqlite.EnsureJobResult{}, err
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	result, err := s.Store.EnsureJob(ctx, sqlite.JobInput{
		Kind:         ReconcileJobKind,
		ResourceType: "github_connection",
		ResourceID:   config.ConnectionID,
		DedupeKey:    ReconcileJobKind + ":" + config.ConnectionID,
		Payload:      payload,
		AvailableAt:  now,
		MaxAttempts:  3,
	})
	if err != nil {
		return sqlite.EnsureJobResult{}, fmt.Errorf("ensure shadow reconciliation job: %w", err)
	}
	return result, nil
}

// ReaderFactory resolves credential reference and returns read-only GitHub
// reader. Config intentionally contains no credential material.
type ReaderFactory func(context.Context, reconcile.Config) (githubadapter.Reader, error)

// Handler executes one durable shadow reconciliation job.
type Handler struct {
	Store     reconcile.Store
	NewReader ReaderFactory
}

// Handle implements worker.Handler. Invalid jobs are permanent failures;
// provider and credential-resolution failures retain original retry class.
func (h Handler) Handle(ctx context.Context, job sqlite.Job) error {
	if job.Kind != ReconcileJobKind {
		return worker.Permanent(errors.New("unexpected reconciliation job kind"))
	}
	config, err := parseJobPayload(job.Payload)
	if err != nil {
		return worker.Permanent(fmt.Errorf("malformed reconciliation job payload: %w", err))
	}
	if h.Store == nil || h.NewReader == nil {
		return worker.Permanent(errors.New("reconciliation handler dependencies are required"))
	}
	reader, err := h.NewReader(ctx, config)
	if err != nil {
		return fmt.Errorf("create GitHub reconciliation reader: %w", err)
	}
	service, err := reconcile.NewService(reader, h.Store)
	if err != nil {
		return worker.Permanent(fmt.Errorf("create reconciliation service: %w", err))
	}
	if _, err := service.Reconcile(ctx, config); err != nil {
		return fmt.Errorf("reconcile GitHub connection: %w", err)
	}
	return nil
}

type reconcileJobPayload struct {
	Version           int    `json:"version"`
	ConnectionID      string `json:"connection_id"`
	APIBaseURL        string `json:"api_base_url"`
	CredentialRefKind string `json:"credential_ref_kind"`
	CredentialLocator string `json:"credential_locator"`
}

func marshalJobPayload(config reconcile.Config) ([]byte, error) {
	if err := validateJobConfig(config); err != nil {
		return nil, err
	}
	return json.Marshal(reconcileJobPayload{
		Version: reconcileJobPayloadVersion, ConnectionID: config.ConnectionID,
		APIBaseURL: config.APIBaseURL, CredentialRefKind: config.CredentialRefKind,
		CredentialLocator: config.CredentialLocator,
	})
}

func parseJobPayload(raw []byte) (reconcile.Config, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload reconcileJobPayload
	if err := decoder.Decode(&payload); err != nil {
		return reconcile.Config{}, errors.New("must be a single supported JSON object")
	}
	if err := requireEOF(decoder); err != nil {
		return reconcile.Config{}, errors.New("must be a single JSON object")
	}
	if payload.Version != reconcileJobPayloadVersion {
		return reconcile.Config{}, fmt.Errorf("unsupported payload version %d", payload.Version)
	}
	config := reconcile.Config{
		ConnectionID: payload.ConnectionID, APIBaseURL: payload.APIBaseURL,
		CredentialRefKind: payload.CredentialRefKind, CredentialLocator: payload.CredentialLocator,
	}
	if err := validateJobConfig(config); err != nil {
		return reconcile.Config{}, err
	}
	return config, nil
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

func validateJobConfig(config reconcile.Config) error {
	if strings.TrimSpace(config.ConnectionID) == "" || strings.TrimSpace(config.APIBaseURL) == "" ||
		strings.TrimSpace(config.CredentialRefKind) == "" || strings.TrimSpace(config.CredentialLocator) == "" {
		return errors.New("connection ID, API URL, and credential reference are required")
	}
	return nil
}

var _ worker.Handler = Handler{}
