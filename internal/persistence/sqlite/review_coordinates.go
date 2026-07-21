package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrReviewPullRequestNotFound means the supplied GitHub coordinate is not an
// active pull request visible through the selected connection.
var ErrReviewPullRequestNotFound = errors.New("review pull request not found")

// ErrReviewProfileVersionNotFound means the supplied immutable profile version
// does not exist.
var ErrReviewProfileVersionNotFound = errors.New("review profile version not found")

// ReviewPullRequestCoordinate is the durable local identity for one GitHub
// pull request. It is safe to use as QueueReviewRun input but contains no
// credential or remote capability.
type ReviewPullRequestCoordinate struct {
	ConnectionID  string
	RepositoryID  string
	PullRequestID string
	Owner         string
	Repository    string
	Number        int
}

// ReviewProfileVersionCoordinate is the durable local identity for one
// immutable profile version.
type ReviewProfileVersionCoordinate struct {
	ProfileID        string
	ProfileVersionID string
	ProfileKey       string
	Version          int
}

// ResolveReviewPullRequest resolves an active, local GitHub coordinate to
// immutable database IDs. It makes no network requests and does not require
// canonical evidence; QueueReviewRun checks that evidence atomically later.
func (s *Store) ResolveReviewPullRequest(ctx context.Context, connectionID, owner, repository string, number int) (ReviewPullRequestCoordinate, error) {
	connectionID = strings.TrimSpace(connectionID)
	owner = strings.TrimSpace(owner)
	repository = strings.TrimSpace(repository)
	if connectionID == "" || owner == "" || repository == "" || strings.Contains(owner, "/") || strings.Contains(repository, "/") || number <= 0 {
		return ReviewPullRequestCoordinate{}, errors.New("review pull request coordinate is invalid")
	}

	var coordinate ReviewPullRequestCoordinate
	err := s.db.QueryRowContext(ctx, `
SELECT connection_repository.connection_id, repository.id, pull_request.id,
       repository.owner_login, repository.name, pull_request.number
FROM connection_repositories AS connection_repository
JOIN connections AS connection ON connection.id = connection_repository.connection_id
JOIN repositories AS repository ON repository.id = connection_repository.repository_id
JOIN pull_requests AS pull_request ON pull_request.repository_id = repository.id
WHERE connection_repository.connection_id = ?
  AND connection.state = 'active'
  AND connection_repository.access_state = 'active'
  AND repository.owner_login = ? COLLATE NOCASE
  AND repository.name = ? COLLATE NOCASE
  AND pull_request.number = ?
  AND pull_request.state = 'open'`, connectionID, owner, repository, number).Scan(
		&coordinate.ConnectionID, &coordinate.RepositoryID, &coordinate.PullRequestID,
		&coordinate.Owner, &coordinate.Repository, &coordinate.Number,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewPullRequestCoordinate{}, ErrReviewPullRequestNotFound
	}
	if err != nil {
		return ReviewPullRequestCoordinate{}, fmt.Errorf("resolve review pull request: %w", err)
	}
	return coordinate, nil
}

// ResolveReviewProfileVersion resolves a human-facing profile key and version
// to immutable database IDs. It is read-only and intentionally does not create
// a profile when a typo is supplied.
func (s *Store) ResolveReviewProfileVersion(ctx context.Context, profileKey string, version int) (ReviewProfileVersionCoordinate, error) {
	profileKey = strings.TrimSpace(profileKey)
	if profileKey == "" || version <= 0 {
		return ReviewProfileVersionCoordinate{}, errors.New("review profile coordinate is invalid")
	}
	var coordinate ReviewProfileVersionCoordinate
	err := s.db.QueryRowContext(ctx, `
SELECT profile.id, profile_version.id, profile.profile_key, profile_version.version
FROM review_profiles AS profile
JOIN review_profile_versions AS profile_version ON profile_version.profile_id = profile.id
WHERE profile.profile_key = ? COLLATE NOCASE
  AND profile_version.version = ?`, profileKey, version).Scan(
		&coordinate.ProfileID, &coordinate.ProfileVersionID, &coordinate.ProfileKey, &coordinate.Version,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewProfileVersionCoordinate{}, ErrReviewProfileVersionNotFound
	}
	if err != nil {
		return ReviewProfileVersionCoordinate{}, fmt.Errorf("resolve review profile version: %w", err)
	}
	return coordinate, nil
}
