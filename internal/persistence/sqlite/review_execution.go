package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sephriot/code-reviewer/internal/application/canonical"
)

// ErrCanonicalReviewTargetNotFound means the requested pull request does not
// currently select complete, verified canonical evidence.
var ErrCanonicalReviewTargetNotFound = errors.New("canonical review target not found")

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

// LoadCurrentCanonicalReviewTarget returns the evidence selected by the
// current projection. It fails closed if stored identity or manifest proof is
// inconsistent, rather than allowing a review to use unverified evidence.
func (s *Store) LoadCurrentCanonicalReviewTarget(ctx context.Context, connectionID, pullRequestID string) (CanonicalReviewTarget, error) {
	if strings.TrimSpace(connectionID) == "" || strings.TrimSpace(pullRequestID) == "" {
		return CanonicalReviewTarget{}, errors.New("canonical review connection and pull request IDs are required")
	}

	var target CanonicalReviewTarget
	var revisionHeadSHA, revisionBaseSHA, revisionManifestSHA string
	err := s.db.QueryRowContext(ctx, `
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
