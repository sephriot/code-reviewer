package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/canonical"
)

// ErrCanonicalReviewTargetNotFound means the requested pull request does not
// currently select complete, verified canonical evidence.
var ErrCanonicalReviewTargetNotFound = errors.New("canonical review target not found")

// ErrReviewRunConflict means an idempotency key is already bound to different
// immutable review facts.
var ErrReviewRunConflict = errors.New("review run idempotency facts conflict")

// CanonicalReviewTarget is the immutable evidence selected for one review.
// It intentionally excludes mutable profile and scheduling choices.
type CanonicalReviewTarget struct {
	ConnectionID   string
	PullRequestID  string
	RepositoryID   string
	ObservationID  string
	RevisionID     string
	ManifestID     string
	HeadSHA        string
	BaseSHA        string
	IdentityKey    string
	ManifestSHA256 string
	ManifestJSON   []byte
	EntryCount     int
}

// PrepareReviewRunInput supplies all immutable choices for an initial review
// execution attempt. Engine configuration is normalized as a JSON object so
// semantically identical configuration uses one durable idempotency identity.
type PrepareReviewRunInput struct {
	ConnectionID      string
	PullRequestID     string
	ProfileID         string
	ProfileVersionID  string
	TriggerKind       string
	TriggerSHA256     string
	UserContextSHA256 string
	CorrelationID     string
	IdempotencyKey    string
	EngineKind        string
	EngineConfigJSON  []byte
	AccessMode        string
	RequestedAt       time.Time
}

// PrepareReviewRunResult identifies durable records selected or created for a
// review execution attempt.
type PrepareReviewRunResult struct {
	IntentID       string
	RunID          string
	RunContextID   string
	IdempotencyKey string
	Created        bool
}

// QueueReviewRunResult identifies the immutable review attempt and its single
// durable execution job. Repeating identical facts returns the same records.
type QueueReviewRunResult struct {
	IntentID       string
	RunID          string
	RunContextID   string
	IdempotencyKey string
	Created        bool
	JobID          string
	JobCreated     bool
}

const reviewExecutionJobKind = "review.execute.v1"

// LoadCurrentCanonicalReviewTarget returns the evidence selected by the
// current projection. It fails closed if stored identity or manifest proof is
// inconsistent, rather than allowing a review to use unverified evidence.
func (s *Store) LoadCurrentCanonicalReviewTarget(ctx context.Context, connectionID, pullRequestID string) (CanonicalReviewTarget, error) {
	return loadCurrentCanonicalReviewTarget(ctx, s.db, connectionID, pullRequestID)
}

type canonicalReviewTargetQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadCurrentCanonicalReviewTarget(ctx context.Context, queryer canonicalReviewTargetQuerier, connectionID, pullRequestID string) (CanonicalReviewTarget, error) {
	if strings.TrimSpace(connectionID) == "" || strings.TrimSpace(pullRequestID) == "" {
		return CanonicalReviewTarget{}, errors.New("canonical review connection and pull request IDs are required")
	}

	var target CanonicalReviewTarget
	var revisionHeadSHA, revisionBaseSHA, revisionManifestSHA string
	err := queryer.QueryRowContext(ctx, `
SELECT projection.connection_id, projection.pull_request_id, projection.repository_id,
       observation.id, revision.id, manifest.id,
       observation.head_sha, observation.base_sha,
       revision.identity_key, revision.head_sha, revision.base_sha, revision.diff_sha256,
       manifest.manifest_sha256, manifest.manifest_json, manifest.entry_count
FROM pull_request_projection_state AS projection
JOIN pull_request_observations AS observation
  ON observation.id = projection.current_observation_id
 AND observation.pull_request_id = projection.pull_request_id
 AND observation.connection_id = projection.connection_id
JOIN revisions AS revision
  ON revision.id = projection.current_revision_id
 AND revision.pull_request_id = projection.pull_request_id
JOIN observation_revision_links AS link
  ON link.observation_id = observation.id
 AND link.pull_request_id = projection.pull_request_id
 AND link.connection_id = projection.connection_id
 AND link.revision_id = revision.id
JOIN revision_manifests AS manifest
  ON manifest.id = link.manifest_id
 AND manifest.revision_id = revision.id
 AND manifest.pull_request_id = projection.pull_request_id
WHERE projection.connection_id = ?
  AND projection.pull_request_id = ?
  AND observation.github_state = 'open'
  AND revision.identity_kind = 'canonical_diff'
  AND revision.is_publishable = 1`, connectionID, pullRequestID).
		Scan(&target.ConnectionID, &target.PullRequestID, &target.RepositoryID,
			&target.ObservationID, &target.RevisionID, &target.ManifestID,
			&target.HeadSHA, &target.BaseSHA,
			&target.IdentityKey, &revisionHeadSHA, &revisionBaseSHA, &revisionManifestSHA,
			&target.ManifestSHA256, &target.ManifestJSON, &target.EntryCount)
	if errors.Is(err, sql.ErrNoRows) {
		return CanonicalReviewTarget{}, ErrCanonicalReviewTargetNotFound
	}
	if err != nil {
		return CanonicalReviewTarget{}, fmt.Errorf("load canonical review target: %w", err)
	}

	verified, err := canonical.Validate(target.ManifestJSON)
	wantIdentityKey := "canonical_diff:v1:" + target.HeadSHA + ":" + target.BaseSHA + ":" + target.ManifestSHA256
	if err != nil ||
		verified.IdentityKey != target.IdentityKey ||
		verified.ManifestSHA256 != target.ManifestSHA256 ||
		verified.ManifestSHA256 != revisionManifestSHA ||
		verified.EntryCount != target.EntryCount ||
		target.IdentityKey != wantIdentityKey ||
		revisionHeadSHA != target.HeadSHA || revisionBaseSHA != target.BaseSHA {
		return CanonicalReviewTarget{}, errors.New("stored canonical review evidence is invalid")
	}
	return target, nil
}

// PrepareReviewRun atomically binds one immutable profile version to the
// current verified canonical target, then creates its first run attempt and
// manifest context. It creates no jobs, domain events, outbox rows, or
// publication work.
func (s *Store) PrepareReviewRun(ctx context.Context, input PrepareReviewRunInput) (PrepareReviewRunResult, error) {
	normalized, err := normalizePrepareReviewRunInput(input)
	if err != nil {
		return PrepareReviewRunResult{}, err
	}

	var result PrepareReviewRunResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		var prepareErr error
		result, prepareErr = prepareReviewRunOnConnection(ctx, conn, normalized)
		return prepareErr
	})
	if err != nil {
		return PrepareReviewRunResult{}, fmt.Errorf("prepare review run: %w", err)
	}
	return result, nil
}

// QueueReviewRun atomically prepares the first immutable review attempt and
// queues precisely one execution job. The job payload carries only the run
// identity; its stable dedupe proof is stored in the job's dedupe_key column.
func (s *Store) QueueReviewRun(ctx context.Context, input PrepareReviewRunInput) (QueueReviewRunResult, error) {
	normalized, err := normalizePrepareReviewRunInput(input)
	if err != nil {
		return QueueReviewRunResult{}, err
	}

	var result QueueReviewRunResult
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		prepared, err := prepareReviewRunOnConnection(ctx, conn, normalized)
		if err != nil {
			return err
		}
		job, err := ensureReviewExecutionJob(ctx, conn, prepared.RunID, normalized.RequestedAt)
		if err != nil {
			return err
		}
		result = QueueReviewRunResult{
			IntentID: prepared.IntentID, RunID: prepared.RunID, RunContextID: prepared.RunContextID,
			IdempotencyKey: prepared.IdempotencyKey, Created: prepared.Created,
			JobID: job.ID, JobCreated: job.Created,
		}
		return nil
	})
	if err != nil {
		return QueueReviewRunResult{}, fmt.Errorf("queue review run: %w", err)
	}
	return result, nil
}

func prepareReviewRunOnConnection(ctx context.Context, conn *sql.Conn, normalized normalizedPrepareReviewRunInput) (PrepareReviewRunResult, error) {
	target, err := loadCurrentCanonicalReviewTarget(ctx, conn, normalized.ConnectionID, normalized.PullRequestID)
	if err != nil {
		return PrepareReviewRunResult{}, err
	}
	if err := requireReviewProfileVersion(ctx, conn, normalized.ProfileID, normalized.ProfileVersionID); err != nil {
		return PrepareReviewRunResult{}, err
	}

	idempotencyKey := normalized.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = reviewRunIdempotencyKey(target, normalized)
	}
	existing, found, err := loadPreparedReviewRun(ctx, conn, idempotencyKey)
	if err != nil {
		return PrepareReviewRunResult{}, err
	}
	if found {
		if !preparedReviewRunMatches(existing, target, normalized) {
			return PrepareReviewRunResult{}, fmt.Errorf("%w: key=%q", ErrReviewRunConflict, idempotencyKey)
		}
		return PrepareReviewRunResult{
			IntentID: existing.IntentID, RunID: existing.RunID, RunContextID: existing.RunContextID,
			IdempotencyKey: idempotencyKey,
		}, nil
	}

	preparedAt := normalized.RequestedAt.UTC().UnixMicro()
	intentID := stableID("review-intent", idempotencyKey)
	runID := stableID("review-run", intentID, "1")
	contextID := stableID("review-run-context", runID)
	if _, err := conn.ExecContext(ctx, `
INSERT INTO review_intents(
 id, connection_id, repository_id, pull_request_id, revision_id, observation_id,
 profile_id, profile_version_id, trigger_kind, idempotency_key, trigger_sha256,
 user_context_sha256, correlation_id, requested_at_us, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
		intentID, target.ConnectionID, target.RepositoryID, target.PullRequestID, target.RevisionID, target.ObservationID,
		normalized.ProfileID, normalized.ProfileVersionID, normalized.TriggerKind, idempotencyKey, normalized.TriggerSHA256,
		normalized.UserContextSHA256, normalized.CorrelationID, preparedAt, preparedAt); err != nil {
		return PrepareReviewRunResult{}, fmt.Errorf("insert review intent: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `
INSERT INTO review_runs(
 id, intent_id, connection_id, pull_request_id, revision_id, observation_id,
 attempt_number, engine_kind, engine_config_json, started_at_us, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		runID, intentID, target.ConnectionID, target.PullRequestID, target.RevisionID, target.ObservationID,
		normalized.EngineKind, normalized.EngineConfigJSON, preparedAt, preparedAt); err != nil {
		return PrepareReviewRunResult{}, fmt.Errorf("insert review run: %w", err)
	}
	for sequence, kind := range []string{"queued", "preparing"} {
		if _, err := conn.ExecContext(ctx, `
INSERT INTO review_run_events(id, run_id, sequence, event_kind, payload_json, occurred_at_us, created_at_us)
VALUES (?, ?, ?, ?, '{}', ?, ?)`,
			stableID("review-run-event", runID, fmt.Sprintf("%d", sequence+1)), runID, sequence+1, kind, preparedAt, preparedAt); err != nil {
			return PrepareReviewRunResult{}, fmt.Errorf("insert review run %s event: %w", kind, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `
INSERT INTO review_run_contexts(
 id, run_id, intent_id, pull_request_id, revision_id, observation_id,
 context_format_version, access_mode, manifest_sha256, manifest_json, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		contextID, runID, intentID, target.PullRequestID, target.RevisionID, target.ObservationID,
		normalized.AccessMode, target.ManifestSHA256, target.ManifestJSON, preparedAt); err != nil {
		return PrepareReviewRunResult{}, fmt.Errorf("insert review run context: %w", err)
	}
	return PrepareReviewRunResult{
		IntentID: intentID, RunID: runID, RunContextID: contextID,
		IdempotencyKey: idempotencyKey, Created: true,
	}, nil
}

func reviewExecutionJobDedupeKey(runID string) string {
	return reviewExecutionJobKind + ":" + runID
}

type reviewExecutionJobPayload struct {
	RunID string `json:"run_id"`
}

func ensureReviewExecutionJob(ctx context.Context, conn *sql.Conn, runID string, availableAt time.Time) (EnsureJobResult, error) {
	dedupeKey := reviewExecutionJobDedupeKey(runID)
	payload, err := json.Marshal(reviewExecutionJobPayload{RunID: runID})
	if err != nil {
		return EnsureJobResult{}, fmt.Errorf("encode review execution job payload: %w", err)
	}

	rows, err := conn.QueryContext(ctx, `
SELECT id, resource_type, resource_id, payload_json
FROM jobs
WHERE kind = ? AND dedupe_key = ?
ORDER BY created_at_us, id`, reviewExecutionJobKind, dedupeKey)
	if err != nil {
		return EnsureJobResult{}, fmt.Errorf("load review execution job: %w", err)
	}
	defer rows.Close()
	var existing struct {
		id           string
		resourceType sql.NullString
		resourceID   sql.NullString
		payload      []byte
	}
	if rows.Next() {
		if err := rows.Scan(&existing.id, &existing.resourceType, &existing.resourceID, &existing.payload); err != nil {
			return EnsureJobResult{}, fmt.Errorf("scan review execution job: %w", err)
		}
		if rows.Next() {
			return EnsureJobResult{}, errors.New("review execution job dedupe is not unique")
		}
		if err := rows.Err(); err != nil {
			return EnsureJobResult{}, fmt.Errorf("iterate review execution jobs: %w", err)
		}
		if existing.resourceType.String != "review_run" || existing.resourceID.String != runID || !bytes.Equal(existing.payload, payload) {
			return EnsureJobResult{}, fmt.Errorf("%w: kind=%q dedupe_key=%q", ErrJobConflict, reviewExecutionJobKind, dedupeKey)
		}
		return EnsureJobResult{ID: existing.id}, nil
	}
	if err := rows.Err(); err != nil {
		return EnsureJobResult{}, fmt.Errorf("iterate review execution jobs: %w", err)
	}

	id, err := newID("job")
	if err != nil {
		return EnsureJobResult{}, err
	}
	job, err := normalizedJobInput(JobInput{
		Kind: reviewExecutionJobKind, ResourceType: "review_run", ResourceID: runID,
		DedupeKey: dedupeKey, Payload: payload, AvailableAt: availableAt, MaxAttempts: 3,
	})
	if err != nil {
		return EnsureJobResult{}, err
	}
	if err := insertJob(ctx, conn, id, job, availableAt.UTC().UnixMicro()); err != nil {
		return EnsureJobResult{}, err
	}
	return EnsureJobResult{ID: id, Created: true}, nil
}

type normalizedPrepareReviewRunInput struct {
	PrepareReviewRunInput
}

func normalizePrepareReviewRunInput(input PrepareReviewRunInput) (normalizedPrepareReviewRunInput, error) {
	input.ConnectionID = strings.TrimSpace(input.ConnectionID)
	input.PullRequestID = strings.TrimSpace(input.PullRequestID)
	input.ProfileID = strings.TrimSpace(input.ProfileID)
	input.ProfileVersionID = strings.TrimSpace(input.ProfileVersionID)
	input.TriggerKind = strings.TrimSpace(input.TriggerKind)
	input.TriggerSHA256 = strings.TrimSpace(input.TriggerSHA256)
	input.UserContextSHA256 = strings.TrimSpace(input.UserContextSHA256)
	input.CorrelationID = strings.TrimSpace(input.CorrelationID)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	input.EngineKind = strings.TrimSpace(input.EngineKind)
	input.AccessMode = strings.TrimSpace(input.AccessMode)
	if input.ConnectionID == "" || input.PullRequestID == "" || input.ProfileID == "" || input.ProfileVersionID == "" ||
		!validReviewTriggerKind(input.TriggerKind) || !validLowerHexDigest(input.TriggerSHA256) ||
		(input.UserContextSHA256 != "" && !validLowerHexDigest(input.UserContextSHA256)) ||
		(input.IdempotencyKey != "" && len(input.IdempotencyKey) > 512) ||
		(input.EngineKind != "cli" && input.EngineKind != "api") || !validReviewAccessMode(input.AccessMode) {
		return normalizedPrepareReviewRunInput{}, errors.New("review run preparation input is invalid")
	}
	config, err := normalizeJSONObject(input.EngineConfigJSON)
	if err != nil {
		return normalizedPrepareReviewRunInput{}, fmt.Errorf("review engine configuration: %w", err)
	}
	input.EngineConfigJSON = config
	if input.RequestedAt.IsZero() {
		input.RequestedAt = time.Now().UTC()
	} else {
		input.RequestedAt = input.RequestedAt.UTC()
	}
	if input.RequestedAt.UnixMicro() < 0 {
		return normalizedPrepareReviewRunInput{}, errors.New("review run requested time is invalid")
	}
	return normalizedPrepareReviewRunInput{PrepareReviewRunInput: input}, nil
}

func requireReviewProfileVersion(ctx context.Context, conn *sql.Conn, profileID, profileVersionID string) error {
	var found string
	err := conn.QueryRowContext(ctx, `
SELECT profile_id FROM review_profile_versions WHERE id = ?`, profileVersionID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("review profile version does not exist")
	}
	if err != nil {
		return fmt.Errorf("load review profile version: %w", err)
	}
	if found != profileID {
		return errors.New("review profile version does not belong to profile")
	}
	return nil
}

type preparedReviewRun struct {
	IntentID          string
	RunID             string
	RunContextID      string
	ConnectionID      string
	RepositoryID      string
	PullRequestID     string
	RevisionID        string
	ObservationID     string
	ProfileID         string
	ProfileVersionID  string
	TriggerKind       string
	TriggerSHA256     string
	UserContextSHA256 sql.NullString
	EngineKind        string
	EngineConfigJSON  []byte
	AccessMode        string
	ManifestSHA256    string
	ManifestJSON      []byte
}

func loadPreparedReviewRun(ctx context.Context, conn *sql.Conn, idempotencyKey string) (preparedReviewRun, bool, error) {
	var value preparedReviewRun
	err := conn.QueryRowContext(ctx, `
SELECT intent.id, run.id, context.id,
       intent.connection_id, intent.repository_id, intent.pull_request_id,
       intent.revision_id, intent.observation_id, intent.profile_id, intent.profile_version_id,
       intent.trigger_kind, intent.trigger_sha256, intent.user_context_sha256,
       run.engine_kind, run.engine_config_json,
       context.access_mode, context.manifest_sha256, context.manifest_json
FROM review_intents AS intent
JOIN review_runs AS run ON run.intent_id = intent.id AND run.attempt_number = 1
JOIN review_run_contexts AS context ON context.run_id = run.id
WHERE intent.idempotency_key = ?`, idempotencyKey).Scan(
		&value.IntentID, &value.RunID, &value.RunContextID,
		&value.ConnectionID, &value.RepositoryID, &value.PullRequestID,
		&value.RevisionID, &value.ObservationID, &value.ProfileID, &value.ProfileVersionID,
		&value.TriggerKind, &value.TriggerSHA256, &value.UserContextSHA256,
		&value.EngineKind, &value.EngineConfigJSON,
		&value.AccessMode, &value.ManifestSHA256, &value.ManifestJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return preparedReviewRun{}, false, nil
	}
	if err != nil {
		return preparedReviewRun{}, false, fmt.Errorf("load prepared review run: %w", err)
	}
	return value, true, nil
}

func preparedReviewRunMatches(existing preparedReviewRun, target CanonicalReviewTarget, input normalizedPrepareReviewRunInput) bool {
	return existing.ConnectionID == target.ConnectionID &&
		existing.RepositoryID == target.RepositoryID &&
		existing.PullRequestID == target.PullRequestID &&
		existing.RevisionID == target.RevisionID &&
		existing.ObservationID == target.ObservationID &&
		existing.ProfileID == input.ProfileID &&
		existing.ProfileVersionID == input.ProfileVersionID &&
		existing.TriggerKind == input.TriggerKind &&
		existing.TriggerSHA256 == input.TriggerSHA256 &&
		existing.UserContextSHA256.String == input.UserContextSHA256 &&
		existing.EngineKind == input.EngineKind &&
		bytes.Equal(existing.EngineConfigJSON, input.EngineConfigJSON) &&
		existing.AccessMode == input.AccessMode &&
		existing.ManifestSHA256 == target.ManifestSHA256 &&
		bytes.Equal(existing.ManifestJSON, target.ManifestJSON)
}

func reviewRunIdempotencyKey(target CanonicalReviewTarget, input normalizedPrepareReviewRunInput) string {
	parts := []string{
		"review-intent:v1", target.ConnectionID, target.PullRequestID, target.RevisionID, target.ObservationID,
		input.ProfileID, input.ProfileVersionID, input.TriggerKind, input.TriggerSHA256,
		input.UserContextSHA256, input.EngineKind, string(input.EngineConfigJSON), input.AccessMode,
	}
	return "review-intent:v1:" + stableID("review-intent-key", parts...)
}

func normalizeJSONObject(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("must be a JSON object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode JSON object: %w", err)
	}
	return encoded, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("must contain one JSON object")
	} else if !errors.Is(err, io.EOF) {
		return errors.New("must contain one JSON object")
	}
	return nil
}

func validReviewTriggerKind(value string) bool {
	return value == "automatic" || value == "manual" || value == "on_demand" || value == "retry"
}

func validReviewAccessMode(value string) bool {
	return value == "diff_only" || value == "selected_files" || value == "read_only_worktree"
}

func validLowerHexDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}
