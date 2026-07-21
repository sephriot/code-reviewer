package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrCanonicalHydrationTargetNotFound means no selected current observation
// matches the requested GitHub pull request identity.
var ErrCanonicalHydrationTargetNotFound = errors.New("canonical hydration target not found")

// CanonicalHydrationTarget is immutable observation identity required before
// fetching external diff evidence.
type CanonicalHydrationTarget struct {
	ConnectionID  string
	ObservationID string
	PullRequestID string
	RepositoryID  string
	Owner         string
	Repository    string
	Number        int
	HeadSHA       string
	BaseSHA       string
}

// ListCanonicalHydrationTargets returns selected observations from active
// repositories whose projection has no canonical current revision.
func (s *Store) ListCanonicalHydrationTargets(ctx context.Context, connectionID string) ([]CanonicalHydrationTarget, error) {
	if strings.TrimSpace(connectionID) == "" {
		return nil, errors.New("canonical hydration connection ID is required")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT observation.connection_id, observation.id, pull_request.id, repository.id,
       repository.owner_login, repository.name, pull_request.number,
       observation.head_sha, observation.base_sha
FROM pull_request_projection_state AS projection
JOIN pull_request_observations AS observation
  ON observation.id = projection.current_observation_id
 AND observation.pull_request_id = projection.pull_request_id
 AND observation.connection_id = projection.connection_id
JOIN pull_requests AS pull_request
  ON pull_request.id = projection.pull_request_id
JOIN repositories AS repository
  ON repository.id = projection.repository_id
JOIN connection_repositories AS connection_repository
  ON connection_repository.connection_id = projection.connection_id
 AND connection_repository.repository_id = projection.repository_id
LEFT JOIN revisions AS revision
  ON revision.id = projection.current_revision_id
WHERE projection.connection_id = ?
  AND connection_repository.access_state = 'active'
  AND (revision.id IS NULL OR revision.identity_kind <> 'canonical_diff')
ORDER BY repository.owner_login COLLATE NOCASE, repository.name COLLATE NOCASE,
         pull_request.number, observation.id`, connectionID)
	if err != nil {
		return nil, fmt.Errorf("list canonical hydration targets: %w", err)
	}
	defer rows.Close()
	var targets []CanonicalHydrationTarget
	for rows.Next() {
		var target CanonicalHydrationTarget
		if err := rows.Scan(&target.ConnectionID, &target.ObservationID, &target.PullRequestID, &target.RepositoryID,
			&target.Owner, &target.Repository, &target.Number, &target.HeadSHA, &target.BaseSHA); err != nil {
			return nil, fmt.Errorf("scan canonical hydration target: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate canonical hydration targets: %w", err)
	}
	return targets, nil
}

// FindCanonicalHydrationTarget resolves only the currently selected immutable
// observation. Superseded observations cannot be hydrated accidentally.
func (s *Store) FindCanonicalHydrationTarget(ctx context.Context, connectionID, owner, repository string, number int) (CanonicalHydrationTarget, error) {
	if connectionID == "" || strings.TrimSpace(owner) == "" || strings.TrimSpace(repository) == "" || strings.Contains(owner, "/") || strings.Contains(repository, "/") || number <= 0 {
		return CanonicalHydrationTarget{}, errors.New("canonical hydration target identity is required")
	}
	var target CanonicalHydrationTarget
	err := s.db.QueryRowContext(ctx, `
SELECT observation.connection_id, observation.id, pull_request.id, repository.id,
       repository.owner_login, repository.name, pull_request.number,
       observation.head_sha, observation.base_sha
FROM pull_request_projection_state AS projection
JOIN pull_request_observations AS observation
  ON observation.id = projection.current_observation_id
 AND observation.pull_request_id = projection.pull_request_id
 AND observation.connection_id = projection.connection_id
JOIN pull_requests AS pull_request
  ON pull_request.id = projection.pull_request_id
JOIN repositories AS repository
  ON repository.id = projection.repository_id
JOIN connection_repositories AS connection_repository
  ON connection_repository.connection_id = projection.connection_id
 AND connection_repository.repository_id = projection.repository_id
WHERE projection.connection_id = ?
  AND repository.owner_login = ? COLLATE NOCASE
  AND repository.name = ? COLLATE NOCASE
  AND pull_request.number = ?
  AND connection_repository.access_state = 'active'`, connectionID, owner, repository, number).
		Scan(&target.ConnectionID, &target.ObservationID, &target.PullRequestID, &target.RepositoryID,
			&target.Owner, &target.Repository, &target.Number, &target.HeadSHA, &target.BaseSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return CanonicalHydrationTarget{}, ErrCanonicalHydrationTargetNotFound
	}
	if err != nil {
		return CanonicalHydrationTarget{}, fmt.Errorf("find canonical hydration target: %w", err)
	}
	return target, nil
}
