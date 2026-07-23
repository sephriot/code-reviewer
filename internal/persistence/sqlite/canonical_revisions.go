package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/canonical"
)

// CanonicalRevisionInput attaches complete canonical proof to the selected
// immutable observation without scheduling or publishing any effect.
type CanonicalRevisionInput struct {
	ConnectionID   string
	ObservationID  string
	HeadSHA        string
	BaseSHA        string
	IdentityKey    string
	ManifestSHA256 string
	ManifestJSON   []byte
	EntryCount     int
	AttachedAt     time.Time
}

// CanonicalRevisionResult names the durable canonical-proof records.
type CanonicalRevisionResult struct {
	RevisionID string
	ManifestID string
	LinkID     string
	Created    bool
}

// AttachCanonicalRevision atomically records canonical proof for the current
// observation. A stale observation or changed SHA fails closed.
func (s *Store) AttachCanonicalRevision(ctx context.Context, input CanonicalRevisionInput) (result CanonicalRevisionResult, err error) {
	if err := validateCanonicalRevisionInput(input); err != nil {
		return result, err
	}
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		var pullRequestID, repositoryID, observationHead, observationBase string
		var embeddedRevisionID sql.NullString
		err := conn.QueryRowContext(ctx, `
SELECT observation.pull_request_id, observation.repository_id, observation.head_sha, observation.base_sha, observation.revision_id
FROM pull_request_observations observation
JOIN pull_request_projection_state projection
  ON projection.pull_request_id = observation.pull_request_id
 AND projection.connection_id = observation.connection_id
WHERE observation.id = ? AND observation.connection_id = ?
  AND projection.current_observation_id = observation.id`, input.ObservationID, input.ConnectionID).
			Scan(&pullRequestID, &repositoryID, &observationHead, &observationBase, &embeddedRevisionID)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("canonical revision requires selected current observation")
		}
		if err != nil {
			return fmt.Errorf("read canonical revision target: %w", err)
		}
		if observationHead != input.HeadSHA || observationBase != input.BaseSHA {
			return errors.New("canonical revision head or base no longer matches observation")
		}

		result.RevisionID = stableID("canonical-revision", pullRequestID, input.IdentityKey)
		result.ManifestID = stableID("revision-manifest", result.RevisionID, input.ManifestSHA256)
		result.LinkID = stableID("observation-revision-link", input.ObservationID, result.RevisionID)
		if embeddedRevisionID.Valid && embeddedRevisionID.String != result.RevisionID {
			return errors.New("observation already embeds a different revision")
		}
		insert, err := conn.ExecContext(ctx, `
INSERT OR IGNORE INTO revisions(
 id, pull_request_id, identity_kind, identity_key, head_sha, base_sha,
 diff_sha256, is_publishable, observed_at_us, created_at_us)
VALUES (?, ?, 'canonical_diff', ?, ?, ?, ?, 1, ?, ?)`, result.RevisionID,
			pullRequestID, input.IdentityKey, input.HeadSHA, input.BaseSHA, input.ManifestSHA256,
			unixMicro(input.AttachedAt), unixMicro(input.AttachedAt))
		if err != nil {
			return fmt.Errorf("insert canonical revision: %w", err)
		}
		_, _ = insert.RowsAffected()
		var identityKey, headSHA, baseSHA, manifestSHA string
		if err := conn.QueryRowContext(ctx, `
SELECT identity_key, head_sha, base_sha, diff_sha256 FROM revisions
WHERE id = ? AND pull_request_id = ?`, result.RevisionID, pullRequestID).
			Scan(&identityKey, &headSHA, &baseSHA, &manifestSHA); err != nil {
			return fmt.Errorf("read canonical revision: %w", err)
		}
		if identityKey != input.IdentityKey || headSHA != input.HeadSHA || baseSHA != input.BaseSHA || manifestSHA != input.ManifestSHA256 {
			return errors.New("canonical revision identity conflict")
		}
		_, err = conn.ExecContext(ctx, `
INSERT OR IGNORE INTO revision_manifests(
 id, revision_id, pull_request_id, manifest_format_version, manifest_sha256,
 entry_count, manifest_json, created_at_us)
VALUES (?, ?, ?, 1, ?, ?, ?, ?)`, result.ManifestID, result.RevisionID,
			pullRequestID, input.ManifestSHA256, input.EntryCount, input.ManifestJSON, unixMicro(input.AttachedAt))
		if err != nil {
			return fmt.Errorf("insert canonical revision manifest: %w", err)
		}
		var existingManifestSHA string
		var existingEntries int
		var existingJSON []byte
		if err := conn.QueryRowContext(ctx, `
SELECT manifest_sha256, entry_count, manifest_json FROM revision_manifests
WHERE id = ? AND revision_id = ? AND pull_request_id = ?`, result.ManifestID, result.RevisionID, pullRequestID).
			Scan(&existingManifestSHA, &existingEntries, &existingJSON); err != nil {
			return fmt.Errorf("read canonical revision manifest: %w", err)
		}
		if existingManifestSHA != input.ManifestSHA256 || existingEntries != input.EntryCount || string(existingJSON) != string(input.ManifestJSON) {
			return errors.New("canonical revision manifest conflict")
		}
		link, err := conn.ExecContext(ctx, `
INSERT OR IGNORE INTO observation_revision_links(
 id, observation_id, pull_request_id, connection_id, revision_id, manifest_id,
 attached_at_us, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, result.LinkID, input.ObservationID, pullRequestID,
			input.ConnectionID, result.RevisionID, result.ManifestID, unixMicro(input.AttachedAt), unixMicro(input.AttachedAt))
		if err != nil {
			return fmt.Errorf("attach canonical revision to observation: %w", err)
		}
		if changed, _ := link.RowsAffected(); changed == 1 {
			result.Created = true
		}
		var linkedRevisionID, linkedManifestID string
		var attachedAtUS int64
		if err := conn.QueryRowContext(ctx, `
SELECT revision_id, manifest_id, attached_at_us FROM observation_revision_links WHERE observation_id = ?`, input.ObservationID).
			Scan(&linkedRevisionID, &linkedManifestID, &attachedAtUS); err != nil {
			return fmt.Errorf("read canonical observation link: %w", err)
		}
		if linkedRevisionID != result.RevisionID || linkedManifestID != result.ManifestID {
			return errors.New("observation already links to a different canonical revision")
		}
		effectiveAttachedAtUS := unixMicro(input.AttachedAt)
		if !result.Created {
			effectiveAttachedAtUS = attachedAtUS
		}
		update, err := conn.ExecContext(ctx, `
UPDATE pull_request_projection_state
SET current_revision_id = ?, updated_at_us = MAX(updated_at_us, ?)
WHERE pull_request_id = ? AND repository_id = ? AND connection_id = ?
  AND current_observation_id = ?`, result.RevisionID, effectiveAttachedAtUS, pullRequestID,
			repositoryID, input.ConnectionID, input.ObservationID)
		if err != nil {
			return fmt.Errorf("advance canonical revision projection: %w", err)
		}
		if changed, _ := update.RowsAffected(); changed != 1 {
			return errors.New("canonical revision target became stale")
		}
		return nil
	})
	return result, err
}

func validateCanonicalRevisionInput(input CanonicalRevisionInput) error {
	if input.ConnectionID == "" || input.ObservationID == "" || input.EntryCount < 0 || input.AttachedAt.IsZero() || input.AttachedAt.UnixMicro() < 0 ||
		!validSHA(input.HeadSHA) || !validSHA(input.BaseSHA) || !validDigest(input.ManifestSHA256) {
		return errors.New("complete canonical revision attachment is required")
	}
	wantPrefix := "canonical_diff:v1:" + input.HeadSHA + ":" + input.BaseSHA + ":" + input.ManifestSHA256
	if input.IdentityKey != wantPrefix {
		return errors.New("canonical revision identity key does not match manifest")
	}
	manifest, err := canonical.Validate(input.ManifestJSON)
	if err != nil || manifest.IdentityKey != input.IdentityKey || manifest.ManifestSHA256 != input.ManifestSHA256 || manifest.EntryCount != input.EntryCount {
		return errors.New("canonical revision manifest does not match attachment")
	}
	return nil
}
