package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrPullRequestDetailNotFound means the requested local pull request is not
// present in the selected connection's current projection.
var ErrPullRequestDetailNotFound = errors.New("pull request detail not found")

// PullRequestDetailQuery identifies one local pull request within its
// connection. It never accepts a remote GitHub coordinate or credential.
type PullRequestDetailQuery struct {
	ConnectionID  string
	PullRequestID string
}

// PullRequestDetail is a compact, current evidence view for one pull request.
// Counts include only immutable records bound to the selected current evidence.
type PullRequestDetail struct {
	ConnectionID  string
	RepositoryID  string
	PullRequestID string
	Owner         string
	Repository    string
	Number        int
	Title         string
	Author        string
	State         string
	HTMLURL       string
	Freshness     string
	LatestFailure string

	CurrentRevisionID           string
	CurrentRevisionIdentityKind string
	CurrentHeadSHA              string
	CurrentBaseSHA              string
	CurrentObservationID        string
	CurrentObservedAt           time.Time

	CurrentReviewRunCount        int
	CurrentProposalRevisionCount int
}

// PullRequestListQuery bounds current observed pull-request inventory.
type PullRequestListQuery struct {
	ConnectionID string
	Limit        int
	Cursor       string
}

// PullRequestListItem is one current observed pull request.
type PullRequestListItem struct {
	ConnectionID  string
	PullRequestID string
	Owner         string
	Repository    string
	Number        int
	Title         string
	State         string
	Freshness     string
	ObservedAt    time.Time
}

// PullRequestListPage is one bounded inventory page.
type PullRequestListPage struct {
	Items      []PullRequestListItem
	NextCursor string
}

// ListPullRequests lists current observed pull requests, including items with
// no active attention.
func (s *Store) ListPullRequests(ctx context.Context, query PullRequestListQuery) (PullRequestListPage, error) {
	connectionID := strings.TrimSpace(query.ConnectionID)
	if len(connectionID) > 256 {
		return PullRequestListPage{}, errors.New("pull request list connection ID is invalid")
	}
	limit, cursor, err := normalizeReadPage(query.Limit, query.Cursor)
	if err != nil {
		return PullRequestListPage{}, err
	}
	hasCursor := 0
	if query.Cursor != "" {
		hasCursor = 1
	}
	rows, err := s.db.QueryContext(ctx, "SELECT projection.connection_id, projection.pull_request_id, repository.owner_login, repository.name, pull_request.number, observation.title, observation.github_state, projection.freshness, observation.observed_at_us FROM pull_request_projection_state AS projection JOIN pull_requests AS pull_request ON pull_request.id = projection.pull_request_id JOIN repositories AS repository ON repository.id = projection.repository_id JOIN pull_request_observations AS observation ON observation.id = projection.current_observation_id WHERE (? = '' OR projection.connection_id = ?) AND (? = 0 OR observation.observed_at_us < ? OR (observation.observed_at_us = ? AND projection.pull_request_id > ?)) ORDER BY observation.observed_at_us DESC, projection.pull_request_id LIMIT ?", connectionID, connectionID, hasCursor, cursor.OccurredAtUS, cursor.OccurredAtUS, cursor.ID, limit+1)
	if err != nil {
		return PullRequestListPage{}, fmt.Errorf("list pull requests: %w", err)
	}
	defer rows.Close()
	page := PullRequestListPage{Items: make([]PullRequestListItem, 0, limit)}
	for rows.Next() {
		var item PullRequestListItem
		var observedAtUS int64
		if err := rows.Scan(&item.ConnectionID, &item.PullRequestID, &item.Owner, &item.Repository, &item.Number, &item.Title, &item.State, &item.Freshness, &observedAtUS); err != nil {
			return PullRequestListPage{}, fmt.Errorf("scan pull request list: %w", err)
		}
		item.ObservedAt = time.UnixMicro(observedAtUS).UTC()
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return PullRequestListPage{}, fmt.Errorf("iterate pull request list: %w", err)
	}
	if len(page.Items) > limit {
		last := page.Items[limit-1]
		next, err := encodeReadCursor(readCursor{OccurredAtUS: last.ObservedAt.UnixMicro(), ID: last.PullRequestID})
		if err != nil {
			return PullRequestListPage{}, err
		}
		page.Items, page.NextCursor = page.Items[:limit], next
	}
	return page, nil
}

// PullRequestDetail loads one current local pull-request projection. It is a
// SELECT-only read model and retains no remote publication capability.
func (s *Store) PullRequestDetail(ctx context.Context, query PullRequestDetailQuery) (PullRequestDetail, error) {
	connectionID, pullRequestID := strings.TrimSpace(query.ConnectionID), strings.TrimSpace(query.PullRequestID)
	if connectionID == "" || pullRequestID == "" {
		return PullRequestDetail{}, errors.New("pull request detail connection and pull request IDs are required")
	}

	var detail PullRequestDetail
	var observedAtUS int64
	err := s.db.QueryRowContext(ctx, `
SELECT projection.connection_id, repository.id, pull_request.id,
       repository.owner_login, repository.name, pull_request.number,
       observation.title, observation.author_login, observation.github_state, COALESCE(pull_request.html_url, ''),
       projection.freshness,
	       COALESCE((SELECT event.event_kind || ':' || json_extract(event.payload_json, '$.code')
	                 FROM review_run_events AS event JOIN review_runs AS run ON run.id = event.run_id
	                 WHERE run.connection_id = projection.connection_id AND run.pull_request_id = projection.pull_request_id
	                   AND run.revision_id = projection.current_revision_id AND run.observation_id = projection.current_observation_id
	                   AND event.event_kind IN ('failed_retryable', 'failed_terminal')
	                 ORDER BY event.occurred_at_us DESC, event.sequence DESC LIMIT 1), ''),
       COALESCE(revision.id, ''), COALESCE(revision.identity_kind, ''),
       COALESCE(revision.head_sha, ''), COALESCE(revision.base_sha, ''),
       observation.id, observation.observed_at_us,
       (SELECT COUNT(*) FROM review_runs AS run
         WHERE run.connection_id = projection.connection_id
           AND run.pull_request_id = projection.pull_request_id
           AND run.revision_id = projection.current_revision_id
           AND run.observation_id = projection.current_observation_id),
       (SELECT COUNT(*) FROM proposal_revisions AS proposal_revision
         JOIN policy_evaluations AS evaluation ON evaluation.id = proposal_revision.policy_evaluation_id
         WHERE evaluation.connection_id = projection.connection_id
           AND proposal_revision.pull_request_id = projection.pull_request_id
           AND proposal_revision.revision_id = projection.current_revision_id
           AND proposal_revision.observation_id = projection.current_observation_id)
FROM pull_request_projection_state AS projection
JOIN pull_requests AS pull_request
  ON pull_request.id = projection.pull_request_id AND pull_request.repository_id = projection.repository_id
JOIN repositories AS repository ON repository.id = projection.repository_id
JOIN pull_request_observations AS observation
  ON observation.id = projection.current_observation_id
 AND observation.pull_request_id = projection.pull_request_id
 AND observation.connection_id = projection.connection_id
LEFT JOIN revisions AS revision
  ON revision.id = projection.current_revision_id AND revision.pull_request_id = projection.pull_request_id
WHERE projection.connection_id = ? AND projection.pull_request_id = ?`, connectionID, pullRequestID).Scan(
		&detail.ConnectionID, &detail.RepositoryID, &detail.PullRequestID,
		&detail.Owner, &detail.Repository, &detail.Number,
		&detail.Title, &detail.Author, &detail.State, &detail.HTMLURL, &detail.Freshness, &detail.LatestFailure,
		&detail.CurrentRevisionID, &detail.CurrentRevisionIdentityKind,
		&detail.CurrentHeadSHA, &detail.CurrentBaseSHA,
		&detail.CurrentObservationID, &observedAtUS,
		&detail.CurrentReviewRunCount, &detail.CurrentProposalRevisionCount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return PullRequestDetail{}, ErrPullRequestDetailNotFound
	}
	if err != nil {
		return PullRequestDetail{}, fmt.Errorf("load pull request detail: %w", err)
	}
	if detail.ConnectionID != connectionID || detail.PullRequestID != pullRequestID ||
		detail.RepositoryID == "" || detail.Owner == "" || detail.Repository == "" ||
		detail.Number <= 0 || detail.Title == "" || detail.State == "" ||
		detail.Freshness == "" || detail.CurrentObservationID == "" || observedAtUS < 0 ||
		detail.CurrentReviewRunCount < 0 || detail.CurrentProposalRevisionCount < 0 {
		return PullRequestDetail{}, errors.New("stored pull request detail is invalid")
	}
	detail.CurrentObservedAt = time.UnixMicro(observedAtUS).UTC()
	return detail, nil
}
