package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sephriot/code-reviewer/internal/application/canonical"
)

// ErrReviewRunExecutionTargetNotFound means a run cannot safely be executed
// from the currently selected canonical evidence.
var ErrReviewRunExecutionTargetNotFound = errors.New("review run execution target not found")

// ReviewExecutionProfile is immutable, verified profile content for one run.
type ReviewExecutionProfile struct {
	ProfileID           string
	ProfileVersionID    string
	ProfileKey          string
	Version             int
	Name                string
	Description         string
	Instructions        string
	OutputSchemaVersion int
	SettingsJSON        []byte
	ContentSHA256       string
}

// ReviewRunExecutionTarget contains the complete read-only facts required to
// execute a queued review run. The caller must still independently re-check
// GitHub evidence before invoking an engine.
type ReviewRunExecutionTarget struct {
	RunID            string
	IntentID         string
	RunContextID     string
	ConnectionID     string
	RepositoryID     string
	PullRequestID    string
	RevisionID       string
	ObservationID    string
	TriggerKind      string
	TriggerSHA256    string
	EngineKind       string
	EngineConfigJSON []byte
	AccessMode       string
	Owner            string
	Repository       string
	Number           int
	Canonical        CanonicalReviewTarget
	Profile          ReviewExecutionProfile

	contextManifestSHA256 string
	contextManifestJSON   []byte
}

// LoadReviewRunExecutionTarget loads one prepared run only when its immutable
// context, profile, and queued/preparing event ledger still agree with the
// current selected canonical evidence. It is read-only and creates no work.
func (s *Store) LoadReviewRunExecutionTarget(ctx context.Context, runID string) (ReviewRunExecutionTarget, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return ReviewRunExecutionTarget{}, errors.New("review run ID is required")
	}

	var target ReviewRunExecutionTarget
	err := s.db.QueryRowContext(ctx, `
SELECT run.id, intent.id, context.id,
       intent.connection_id, intent.repository_id, intent.pull_request_id,
       intent.revision_id, intent.observation_id, intent.trigger_kind, intent.trigger_sha256,
	       run.engine_kind, run.engine_config_json, context.access_mode,
	       context.manifest_sha256, context.manifest_json,
       repository.owner_login, repository.name, pull_request.number,
       profile.id, version.id, profile.profile_key, version.version,
       version.name, version.description, version.instructions, version.output_schema_version,
       version.settings_json, version.content_sha256
FROM review_runs AS run
JOIN review_intents AS intent ON intent.id = run.intent_id
JOIN review_run_contexts AS context ON context.run_id = run.id
JOIN pull_requests AS pull_request ON pull_request.id = intent.pull_request_id
JOIN repositories AS repository ON repository.id = intent.repository_id
JOIN review_profiles AS profile ON profile.id = intent.profile_id
JOIN review_profile_versions AS version
  ON version.id = intent.profile_version_id AND version.profile_id = intent.profile_id
WHERE run.id = ?
  AND NOT EXISTS (SELECT 1 FROM assessments WHERE assessments.run_id = run.id)`, runID).Scan(
		&target.RunID, &target.IntentID, &target.RunContextID,
		&target.ConnectionID, &target.RepositoryID, &target.PullRequestID,
		&target.RevisionID, &target.ObservationID, &target.TriggerKind, &target.TriggerSHA256,
		&target.EngineKind, &target.EngineConfigJSON, &target.AccessMode,
		&target.contextManifestSHA256, &target.contextManifestJSON,
		&target.Owner, &target.Repository, &target.Number,
		&target.Profile.ProfileID, &target.Profile.ProfileVersionID, &target.Profile.ProfileKey, &target.Profile.Version,
		&target.Profile.Name, &target.Profile.Description, &target.Profile.Instructions, &target.Profile.OutputSchemaVersion,
		&target.Profile.SettingsJSON, &target.Profile.ContentSHA256,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRunExecutionTarget{}, ErrReviewRunExecutionTargetNotFound
	}
	if err != nil {
		return ReviewRunExecutionTarget{}, fmt.Errorf("load review run execution target: %w", err)
	}
	if err := validateReviewRunExecutionFacts(ctx, s.db, target); err != nil {
		return ReviewRunExecutionTarget{}, err
	}
	current, err := loadCurrentCanonicalReviewTarget(ctx, s.db, target.ConnectionID, target.PullRequestID)
	if errors.Is(err, ErrCanonicalReviewTargetNotFound) {
		return ReviewRunExecutionTarget{}, ErrReviewRunExecutionTargetNotFound
	}
	if err != nil {
		return ReviewRunExecutionTarget{}, fmt.Errorf("load current review run evidence: %w", err)
	}
	if !executionTargetMatchesCurrent(target, current) {
		return ReviewRunExecutionTarget{}, errors.New("review run context differs from current canonical evidence")
	}
	target.Canonical = current
	target.EngineConfigJSON = append([]byte(nil), target.EngineConfigJSON...)
	target.Profile.SettingsJSON = append([]byte(nil), target.Profile.SettingsJSON...)
	target.contextManifestJSON = append([]byte(nil), target.contextManifestJSON...)
	return target, nil
}

type reviewRunExecutionQuerier interface {
	canonicalReviewTargetQuerier
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func validateReviewRunExecutionFacts(ctx context.Context, queryer reviewRunExecutionQuerier, target ReviewRunExecutionTarget) error {
	if target.RunID == "" || target.IntentID == "" || target.RunContextID == "" ||
		target.ConnectionID == "" || target.RepositoryID == "" || target.PullRequestID == "" ||
		target.RevisionID == "" || target.ObservationID == "" || target.Owner == "" ||
		target.Repository == "" || strings.Contains(target.Owner, "/") || strings.Contains(target.Repository, "/") ||
		target.Number <= 0 || !validReviewTriggerKind(target.TriggerKind) || !validLowerHexDigest(target.TriggerSHA256) ||
		(target.EngineKind != "cli" && target.EngineKind != "api") || !validReviewAccessMode(target.AccessMode) {
		return errors.New("review run execution facts are invalid")
	}
	engineConfig, err := normalizeJSONObject(target.EngineConfigJSON)
	if err != nil || !bytes.Equal(engineConfig, target.EngineConfigJSON) {
		return errors.New("review run engine configuration is invalid")
	}
	if err := validateReviewExecutionProfile(target.Profile); err != nil {
		return err
	}
	contextRevision, err := canonical.Validate(target.contextManifestJSON)
	if err != nil || contextRevision.ManifestSHA256 != target.contextManifestSHA256 ||
		!bytes.Equal(contextRevision.Manifest, target.contextManifestJSON) {
		return errors.New("review run manifest context is invalid")
	}
	return requirePreparedRunEvents(ctx, queryer, target.RunID)
}

func validateReviewExecutionProfile(profile ReviewExecutionProfile) error {
	if profile.OutputSchemaVersion != 1 {
		return errors.New("review profile output schema version is invalid")
	}
	normalized, err := normalizeReviewProfileVersionInput(CreateReviewProfileVersionInput{
		ProfileKey: profile.ProfileKey, Version: profile.Version, Name: profile.Name,
		Description: profile.Description, Instructions: profile.Instructions, SettingsJSON: profile.SettingsJSON,
	})
	if err != nil || normalized.ProfileKey != profile.ProfileKey || normalized.Name != profile.Name ||
		normalized.Description != profile.Description || normalized.Instructions != profile.Instructions ||
		!bytes.Equal(normalized.SettingsJSON, profile.SettingsJSON) || normalized.ContentSHA256 != profile.ContentSHA256 {
		return errors.New("review profile content is invalid")
	}
	return nil
}

func requirePreparedRunEvents(ctx context.Context, queryer reviewRunExecutionQuerier, runID string) error {
	rows, err := queryer.QueryContext(ctx, `SELECT sequence, event_kind, payload_json FROM review_run_events WHERE run_id = ? ORDER BY sequence`, runID)
	if err != nil {
		return fmt.Errorf("load review run events: %w", err)
	}
	defer rows.Close()
	want := []string{"queued", "preparing"}
	for index, kind := range want {
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return fmt.Errorf("iterate review run events: %w", err)
			}
			return errors.New("review run is not queued and preparing")
		}
		var sequence int
		var eventKind string
		var payload []byte
		if err := rows.Scan(&sequence, &eventKind, &payload); err != nil {
			return fmt.Errorf("scan review run event: %w", err)
		}
		if sequence != index+1 || eventKind != kind || !bytes.Equal(payload, []byte(`{}`)) {
			return errors.New("review run event ledger is invalid")
		}
	}
	if rows.Next() {
		return errors.New("review run is no longer preparing")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate review run events: %w", err)
	}
	return nil
}

func executionTargetMatchesCurrent(target ReviewRunExecutionTarget, current CanonicalReviewTarget) bool {
	return target.ConnectionID == current.ConnectionID &&
		target.RepositoryID == current.RepositoryID &&
		target.PullRequestID == current.PullRequestID &&
		target.RevisionID == current.RevisionID &&
		target.ObservationID == current.ObservationID &&
		target.contextManifestSHA256 == current.ManifestSHA256 &&
		bytes.Equal(target.contextManifestJSON, current.ManifestJSON)
}
