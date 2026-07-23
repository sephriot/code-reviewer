package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/reconcile"
)

var _ reconcile.Store = (*Store)(nil)

// UpsertGitHubConnection inserts a verified connection or refreshes the same immutable identity.
func (s *Store) UpsertGitHubConnection(ctx context.Context, input reconcile.ConnectionInput) error {
	if err := validateConnection(input); err != nil {
		return err
	}
	return withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		var apiBaseURL, accountNodeID, credentialKind, credentialLocator string
		var accountDatabaseID int64
		err := conn.QueryRowContext(ctx, `
SELECT api_base_url, COALESCE(account_node_id, ''), account_database_id,
       credential_ref_kind, credential_locator
FROM connections WHERE id = ?`, input.ID).Scan(
			&apiBaseURL, &accountNodeID, &accountDatabaseID, &credentialKind, &credentialLocator,
		)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			authKind := "fine_grained_pat"
			if input.CredentialRefKind == "github_cli" {
				authKind = "github_cli"
			}
			_, err = conn.ExecContext(ctx, `
INSERT INTO connections(
 id, provider, mode, auth_kind, api_base_url, account_login, account_node_id,
 account_database_id, credential_ref_kind, credential_locator, state,
 permissions_json, last_checked_at_us, created_at_us, updated_at_us)
VALUES (?, 'github', 'local_user', ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?)`,
				input.ID, authKind, input.APIBaseURL, input.AccountLogin, nullString(input.AccountNodeID),
				input.AccountDatabaseID, input.CredentialRefKind, input.CredentialLocator,
				input.PermissionsJSON, unixMicro(input.CheckedAt), unixMicro(input.CheckedAt), unixMicro(input.CheckedAt),
			)
			if err != nil {
				return fmt.Errorf("insert GitHub connection: %w", err)
			}
			return nil
		case err != nil:
			return fmt.Errorf("read GitHub connection: %w", err)
		}
		if apiBaseURL != input.APIBaseURL || credentialKind != input.CredentialRefKind || credentialLocator != input.CredentialLocator {
			return errors.New("GitHub connection credential coordinates changed")
		}
		if accountDatabaseID != input.AccountDatabaseID || (accountNodeID != "" && input.AccountNodeID != "" && accountNodeID != input.AccountNodeID) {
			return errors.New("GitHub connection account identity changed")
		}
		_, err = conn.ExecContext(ctx, `
UPDATE connections
SET account_login = ?, account_node_id = COALESCE(account_node_id, ?), state = 'active',
    permissions_json = ?, last_checked_at_us = ?, updated_at_us = ?
WHERE id = ?`, input.AccountLogin, nullString(input.AccountNodeID), input.PermissionsJSON,
			unixMicro(input.CheckedAt), unixMicro(input.CheckedAt), input.ID)
		if err != nil {
			return fmt.Errorf("refresh GitHub connection: %w", err)
		}
		return nil
	})
}

// NextReconciliationGeneration reserves one monotonically increasing scope generation.
func (s *Store) NextReconciliationGeneration(ctx context.Context, scope reconcile.Scope, startedAt time.Time) (result reconcile.Generation, err error) {
	if err := validateScope(scope); err != nil {
		return result, err
	}
	if startedAt.IsZero() || startedAt.UnixMicro() < 0 {
		return result, errors.New("generation start time is required")
	}
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		var accountLogin, state string
		if err := conn.QueryRowContext(ctx, `SELECT account_login, state FROM connections WHERE id = ?`, scope.ConnectionID).Scan(&accountLogin, &state); err != nil {
			return fmt.Errorf("read reconciliation connection: %w", err)
		}
		if state != "active" || !strings.EqualFold(accountLogin, scope.Key) {
			return errors.New("reconciliation scope does not match active connection account")
		}
		if err := conn.QueryRowContext(ctx, `
SELECT COALESCE(MAX(generation_number), 0) + 1
FROM reconciliation_generations
WHERE connection_id = ? AND scope_kind = ? AND scope_key = ? COLLATE NOCASE AND query_partition = ?`,
			scope.ConnectionID, scope.Kind, scope.Key, scope.QueryPartition).Scan(&result.Number); err != nil {
			return fmt.Errorf("reserve reconciliation generation number: %w", err)
		}
		result.Scope = scope
		result.ID = stableID("generation", scope.ConnectionID, string(scope.Kind), strings.ToLower(scope.Key), scope.QueryPartition, fmt.Sprint(result.Number))
		_, err := conn.ExecContext(ctx, `
INSERT INTO reconciliation_generations(
 id, connection_id, scope_kind, scope_key, query_partition, generation_number,
 mode, state, pages_received, result_count, started_at_us)
VALUES (?, ?, ?, ?, ?, ?, 'shadow_read_only', 'running', 0, 0, ?)`,
			result.ID, scope.ConnectionID, scope.Kind, scope.Key, scope.QueryPartition, result.Number, unixMicro(startedAt))
		if err != nil {
			return fmt.Errorf("insert reconciliation generation: %w", err)
		}
		return nil
	})
	return result, err
}

// ListActiveRelationships returns direct-refresh candidates for an exact scope.
func (s *Store) ListActiveRelationships(ctx context.Context, scope reconcile.Scope) ([]reconcile.ActiveRelationship, error) {
	if err := validateScope(scope); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT relationship.id, relationship.relationship_kind, connection.account_login,
       relationship.subject_database_id, repository.owner_login, repository.name,
	   pull_request.number, observation.github_updated_at_us
FROM pr_relationships relationship
JOIN connections connection ON connection.id = relationship.connection_id
JOIN repositories repository ON repository.id = relationship.repository_id
JOIN pull_requests pull_request ON pull_request.id = relationship.pull_request_id
	JOIN pull_request_projection_state projection ON projection.pull_request_id = pull_request.id
	JOIN pull_request_observations observation ON observation.id = projection.current_observation_id
WHERE relationship.connection_id = ? AND relationship.active_until_us IS NULL
  AND relationship.relationship_kind = ?
  AND relationship.subject_database_id = connection.account_database_id
  AND connection.account_login = ? COLLATE NOCASE
ORDER BY repository.full_name COLLATE NOCASE, pull_request.number, relationship.id`,
		scope.ConnectionID, relationshipKindForScope(scope.Kind), scope.Key)
	if err != nil {
		return nil, fmt.Errorf("list active relationships: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []reconcile.ActiveRelationship
	for rows.Next() {
		var item reconcile.ActiveRelationship
		var githubUpdatedAtUS int64
		if err := rows.Scan(&item.ID, &item.Kind, &item.SubjectLogin, &item.SubjectDatabaseID,
			&item.RepositoryOwner, &item.RepositoryName, &item.PullRequestNumber, &githubUpdatedAtUS); err != nil {
			return nil, fmt.Errorf("scan active relationship: %w", err)
		}
		item.CurrentGitHubUpdatedAt = time.UnixMicro(githubUpdatedAtUS).UTC()
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active relationships: %w", err)
	}
	return result, nil
}

// ApplyReconciliationGeneration atomically finalizes and projects one shadow generation.
func (s *Store) ApplyReconciliationGeneration(ctx context.Context, input reconcile.ApplyGenerationInput) (result reconcile.ApplyGenerationResult, err error) {
	if err := validateApplyInput(input); err != nil {
		return result, err
	}
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		if err := verifyRunningGeneration(ctx, conn, input.Generation); err != nil {
			return err
		}
		resolved := make([]resolvedProjection, 0, len(input.Items))
		pullRequests := make(map[string]struct{}, len(input.Items))
		for _, item := range input.Items {
			projection, counts, err := resolveProjection(ctx, conn, input.Generation, item, input.FinishedAt)
			if err != nil {
				return err
			}
			if _, duplicate := pullRequests[projection.pullRequestID]; duplicate {
				return errors.New("generation contains duplicate pull request")
			}
			pullRequests[projection.pullRequestID] = struct{}{}
			result.NewRepositories += counts.NewRepositories
			result.NewPullRequests += counts.NewPullRequests
			resolved = append(resolved, projection)
		}
		coverage := ""
		if input.State == reconcile.GenerationComplete {
			coverage = generationCoverage(resolved)
		}
		if err := finalizeGeneration(ctx, conn, input, coverage, len(resolved)); err != nil {
			return err
		}
		closureIDs := make(map[string]struct{}, len(input.Closures))
		for _, closure := range input.Closures {
			if _, duplicate := closureIDs[closure.RelationshipID]; duplicate {
				return errors.New("generation contains duplicate relationship closure")
			}
			closureIDs[closure.RelationshipID] = struct{}{}
		}
		for index := range resolved {
			counts, relationshipID, err := persistProjection(ctx, conn, input.Generation, resolved[index], input.FinishedAt,
				input.State == reconcile.GenerationComplete)
			if err != nil {
				return err
			}
			if _, contradictory := closureIDs[relationshipID]; contradictory {
				return errors.New("generation cannot open and close the same relationship")
			}
			result.NewObservations += counts.NewObservations
			result.OpenedRelationships += counts.OpenedRelationships
			result.ClosedRelationships += counts.ClosedRelationships
		}
		for _, closure := range input.Closures {
			closed, err := closeRelationship(ctx, conn, input.Generation, closure, input.FinishedAt)
			if err != nil {
				return err
			}
			result.ClosedRelationships += closed
		}
		if err := updateCheckpoint(ctx, conn, input.Generation, input.State, input.FinishedAt); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

type resolvedProjection struct {
	item                        reconcile.ProjectionItem
	repositoryID, pullRequestID string
	observationID               string
}

func resolveProjection(ctx context.Context, conn *sql.Conn, generation reconcile.Generation, item reconcile.ProjectionItem, now time.Time) (resolvedProjection, reconcile.ApplyGenerationResult, error) {
	var result resolvedProjection
	var counts reconcile.ApplyGenerationResult
	result.item = item
	repositoryID, created, err := resolveRepository(ctx, conn, item.Repository, now)
	if err != nil {
		return result, counts, err
	}
	result.repositoryID = repositoryID
	if created {
		counts.NewRepositories++
	}
	var installationID sql.NullInt64
	var permissions []byte
	if err := conn.QueryRowContext(ctx, `SELECT installation_id, permissions_json FROM connections WHERE id = ?`, generation.Scope.ConnectionID).Scan(&installationID, &permissions); err != nil {
		return result, counts, fmt.Errorf("read connection repository attributes: %w", err)
	}
	_, err = conn.ExecContext(ctx, `
INSERT INTO connection_repositories(
 connection_id, repository_id, github_repository_id, github_node_id, installation_id,
 access_state, permissions_json, last_seen_generation_id, created_at_us, updated_at_us)
VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?, ?)
ON CONFLICT(connection_id, repository_id) DO UPDATE SET
 access_state = 'active', permissions_json = excluded.permissions_json,
 last_seen_generation_id = excluded.last_seen_generation_id, updated_at_us = excluded.updated_at_us`,
		generation.Scope.ConnectionID, repositoryID, item.Repository.GitHubID, item.Repository.NodeID,
		installationID, permissions, generation.ID, unixMicro(now), unixMicro(now))
	if err != nil {
		return result, counts, fmt.Errorf("attach repository to connection: %w", err)
	}
	pullRequestID, created, err := resolvePullRequest(ctx, conn, repositoryID, item.PullRequest, now)
	if err != nil {
		return result, counts, err
	}
	result.pullRequestID = pullRequestID
	if created {
		counts.NewPullRequests++
	}
	result.observationID = stableID("observation", pullRequestID, item.PullRequest.FactsSHA256)
	var existingObservationID, existingConnectionID string
	err = conn.QueryRowContext(ctx, `
SELECT id, connection_id FROM pull_request_observations
WHERE pull_request_id = ? AND facts_format_version = 1 AND facts_sha256 = ?`,
		pullRequestID, item.PullRequest.FactsSHA256).Scan(&existingObservationID, &existingConnectionID)
	if err == nil {
		if existingConnectionID != generation.Scope.ConnectionID {
			return result, counts, errors.New("observation identity belongs to another connection")
		}
		result.observationID = existingObservationID
	} else if !errors.Is(err, sql.ErrNoRows) {
		return result, counts, fmt.Errorf("read existing pull request observation: %w", err)
	}
	return result, counts, nil
}

func resolveRepository(ctx context.Context, conn *sql.Conn, facts reconcile.RepositoryFacts, now time.Time) (string, bool, error) {
	rows, err := conn.QueryContext(ctx, `
SELECT id, COALESCE(github_id, 0), COALESCE(github_node_id, '')
FROM repositories
WHERE full_name = ? COLLATE NOCASE OR github_id = ? OR github_node_id = ?`, facts.FullName, facts.GitHubID, facts.NodeID)
	if err != nil {
		return "", false, fmt.Errorf("find repository identity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type identity struct {
		id, nodeID string
		githubID   int64
	}
	var identities []identity
	for rows.Next() {
		var item identity
		if err := rows.Scan(&item.id, &item.githubID, &item.nodeID); err != nil {
			return "", false, fmt.Errorf("scan repository identity: %w", err)
		}
		identities = append(identities, item)
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("iterate repository identities: %w", err)
	}
	owner, name, _ := strings.Cut(facts.FullName, "/")
	if len(identities) == 0 {
		id := stableID("repository", fmt.Sprint(facts.GitHubID))
		_, err := conn.ExecContext(ctx, `
INSERT INTO repositories(id, github_node_id, full_name, owner_login, name, created_at_us, updated_at_us, github_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, facts.NodeID, facts.FullName, owner, name, unixMicro(now), unixMicro(now), facts.GitHubID)
		if err != nil {
			return "", false, fmt.Errorf("insert repository: %w", err)
		}
		return id, true, nil
	}
	if len(identities) != 1 {
		return "", false, errors.New("repository identity collision")
	}
	selected := identities[0]
	if (selected.githubID != 0 && selected.githubID != facts.GitHubID) || (selected.nodeID != "" && selected.nodeID != facts.NodeID) {
		return "", false, errors.New("repository immutable GitHub identity changed")
	}
	_, err = conn.ExecContext(ctx, `
UPDATE repositories
SET github_id = COALESCE(github_id, ?), github_node_id = COALESCE(github_node_id, ?),
    full_name = ?, owner_login = ?, name = ?, updated_at_us = ?
	WHERE id = ?`, facts.GitHubID, facts.NodeID, facts.FullName, owner, name, unixMicro(now), selected.id)
	if err != nil {
		return "", false, fmt.Errorf("bridge repository identity: %w", err)
	}
	return selected.id, false, nil
}

func resolvePullRequest(ctx context.Context, conn *sql.Conn, repositoryID string, facts reconcile.PullRequestFacts, now time.Time) (string, bool, error) {
	rows, err := conn.QueryContext(ctx, `
SELECT id, repository_id, COALESCE(github_id, 0)
FROM pull_requests WHERE (repository_id = ? AND number = ?) OR github_id = ?`, repositoryID, facts.Number, facts.GitHubID)
	if err != nil {
		return "", false, fmt.Errorf("find pull request identity: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type identity struct {
		id, repositoryID string
		githubID         int64
	}
	var identities []identity
	for rows.Next() {
		var item identity
		if err := rows.Scan(&item.id, &item.repositoryID, &item.githubID); err != nil {
			return "", false, fmt.Errorf("scan pull request identity: %w", err)
		}
		identities = append(identities, item)
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("iterate pull request identities: %w", err)
	}
	if len(identities) == 0 {
		id := stableID("pull-request", fmt.Sprint(facts.GitHubID))
		_, err := conn.ExecContext(ctx, `
INSERT INTO pull_requests(id, repository_id, github_id, number, title, author_login, html_url, state, created_at_us, updated_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, repositoryID, facts.GitHubID, facts.Number, facts.Title,
			facts.AuthorLogin, facts.URL, facts.State, unixMicro(now), unixMicro(now))
		if err != nil {
			return "", false, fmt.Errorf("insert pull request: %w", err)
		}
		return id, true, nil
	}
	if len(identities) != 1 {
		return "", false, errors.New("pull request identity collision")
	}
	selected := identities[0]
	if selected.repositoryID != repositoryID || (selected.githubID != 0 && selected.githubID != facts.GitHubID) {
		return "", false, errors.New("pull request immutable GitHub identity changed")
	}
	_, err = conn.ExecContext(ctx, `
UPDATE pull_requests SET github_id = COALESCE(github_id, ?), title = ?, author_login = ?, html_url = ?, state = ?, updated_at_us = ?
	WHERE id = ?`, facts.GitHubID, facts.Title, facts.AuthorLogin, facts.URL, facts.State, unixMicro(now), selected.id)
	if err != nil {
		return "", false, fmt.Errorf("bridge pull request identity: %w", err)
	}
	return selected.id, false, nil
}

func finalizeGeneration(ctx context.Context, conn *sql.Conn, input reconcile.ApplyGenerationInput, coverage string, resultCount int) error {
	var coverageValue any
	if coverage != "" {
		coverageValue = coverage
	}
	_, err := conn.ExecContext(ctx, `
UPDATE reconciliation_generations
SET state = ?, pages_expected = ?, pages_received = ?, provider_incomplete_results = ?,
    provider_total = ?, result_count = ?, coverage_sha256 = ?, error_class = ?, error_message = ?, finished_at_us = ?
WHERE id = ? AND state = 'running'`, input.State, input.PagesExpected, input.PagesReceived, boolInt(input.ProviderIncomplete),
		input.ProviderTotal, resultCount, coverageValue, nullString(input.ErrorClass), nullString(input.ErrorMessage),
		unixMicro(input.FinishedAt), input.Generation.ID)
	if err != nil {
		return fmt.Errorf("finalize reconciliation generation: %w", err)
	}
	return nil
}

func persistProjection(ctx context.Context, conn *sql.Conn, generation reconcile.Generation, projection resolvedProjection, now time.Time, complete bool) (reconcile.ApplyGenerationResult, string, error) {
	var result reconcile.ApplyGenerationResult
	facts := projection.item.PullRequest
	sourceKind := projection.item.ObservationSource
	var sourceGenerationID any
	if sourceKind == reconcile.ObservationReconciliation {
		sourceGenerationID = generation.ID
	}
	insert, err := conn.ExecContext(ctx, `
INSERT OR IGNORE INTO pull_request_observations(
 id, connection_id, repository_id, pull_request_id, revision_id, head_sha, base_sha,
 source_kind, source_generation_id, source_priority, facts_format_version, facts_sha256,
 title, author_login, author_database_id, body_sha256, labels_json, is_draft, base_ref,
 requested_reviewers_json, relationship_set_json, github_state, github_updated_at_us, observed_at_us, created_at_us)
VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projection.observationID, generation.Scope.ConnectionID, projection.repositoryID, projection.pullRequestID,
		facts.HeadSHA, facts.BaseSHA, sourceKind, sourceGenerationID, observationPriority(sourceKind), facts.FactsSHA256, facts.Title, facts.AuthorLogin,
		facts.AuthorDatabaseID, facts.BodySHA256, facts.LabelsJSON, boolInt(facts.IsDraft), facts.BaseRef,
		facts.RequestedReviewersJSON, facts.RelationshipSetJSON, facts.State, unixMicro(facts.GitHubUpdatedAt), unixMicro(now), unixMicro(now))
	if err != nil {
		return result, "", fmt.Errorf("insert pull request observation: %w", err)
	}
	if affected, _ := insert.RowsAffected(); affected == 1 {
		result.NewObservations++
	}
	var observationID, observationConnectionID string
	if err := conn.QueryRowContext(ctx, `
SELECT id, connection_id FROM pull_request_observations
WHERE pull_request_id = ? AND facts_format_version = 1 AND facts_sha256 = ?`,
		projection.pullRequestID, facts.FactsSHA256).Scan(&observationID, &observationConnectionID); err != nil {
		return result, "", fmt.Errorf("read pull request observation: %w", err)
	}
	if observationConnectionID != generation.Scope.ConnectionID {
		return result, "", errors.New("observation identity belongs to another connection")
	}
	projection.observationID = observationID
	if _, err := conn.ExecContext(ctx, `
INSERT INTO reconciliation_generation_items(
 generation_id, connection_id, repository_id, pull_request_id, observation_id, recorded_at_us)
VALUES (?, ?, ?, ?, ?, ?)`, generation.ID, generation.Scope.ConnectionID, projection.repositoryID,
		projection.pullRequestID, observationID, unixMicro(now)); err != nil {
		return result, "", fmt.Errorf("insert generation membership: %w", err)
	}
	relationshipID := ""
	if projection.item.RelationshipKind == "" {
		if err := updateProjectionState(ctx, conn, generation, projection, observationID, now, complete); err != nil {
			return result, "", err
		}
		return result, "", nil
	}
	var existingSubjectLogin string
	err = conn.QueryRowContext(ctx, `
SELECT id, subject_login FROM pr_relationships
WHERE connection_id = ? AND pull_request_id = ? AND relationship_kind = ?
  AND subject_database_id = ? AND active_until_us IS NULL`, generation.Scope.ConnectionID,
		projection.pullRequestID, projection.item.RelationshipKind, projection.item.SubjectDatabaseID).
		Scan(&relationshipID, &existingSubjectLogin)
	if errors.Is(err, sql.ErrNoRows) {
		relationshipID, err = openRelationship(ctx, conn, generation, projection, observationID, now)
		if err != nil {
			return result, "", err
		}
		result.OpenedRelationships++
	} else if err != nil {
		return result, "", fmt.Errorf("read active pull request relationship: %w", err)
	} else if !strings.EqualFold(existingSubjectLogin, projection.item.SubjectLogin) {
		if _, err := conn.ExecContext(ctx, `
UPDATE pr_relationships
SET active_until_us = ?, ended_by_observation_id = ?, updated_at_us = ?
WHERE id = ? AND active_until_us IS NULL`, unixMicro(now), observationID, unixMicro(now), relationshipID); err != nil {
			return result, "", fmt.Errorf("end renamed pull request relationship: %w", err)
		}
		relationshipID, err = openRelationship(ctx, conn, generation, projection, observationID, now)
		if err != nil {
			return result, "", err
		}
		result.ClosedRelationships++
		result.OpenedRelationships++
	}
	if err := updateProjectionState(ctx, conn, generation, projection, observationID, now, complete); err != nil {
		return result, "", err
	}
	return result, relationshipID, nil
}

func openRelationship(ctx context.Context, conn *sql.Conn, generation reconcile.Generation, projection resolvedProjection, observationID string, now time.Time) (string, error) {
	relationshipID := stableID("relationship", generation.Scope.ConnectionID, projection.pullRequestID,
		string(projection.item.RelationshipKind), fmt.Sprint(projection.item.SubjectDatabaseID), generation.ID)
	sourceKind := string(projection.item.ObservationSource)
	var startedGenerationID any
	if projection.item.ObservationSource == reconcile.ObservationReconciliation {
		startedGenerationID = generation.ID
	}
	_, err := conn.ExecContext(ctx, `
INSERT INTO pr_relationships(
 id, connection_id, repository_id, pull_request_id, relationship_kind,
 subject_database_id, subject_login, source_kind, started_observation_id,
 started_generation_id, active_from_us, created_at_us, updated_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, relationshipID,
		generation.Scope.ConnectionID, projection.repositoryID, projection.pullRequestID,
		projection.item.RelationshipKind, projection.item.SubjectDatabaseID, projection.item.SubjectLogin,
		sourceKind, observationID, startedGenerationID, unixMicro(now), unixMicro(now), unixMicro(now))
	if err != nil {
		return "", fmt.Errorf("open pull request relationship: %w", err)
	}
	return relationshipID, nil
}

func updateProjectionState(ctx context.Context, conn *sql.Conn, generation reconcile.Generation, projection resolvedProjection, observationID string, now time.Time, complete bool) error {
	var completeGeneration any
	if complete {
		completeGeneration = generation.ID
	}
	_, err := conn.ExecContext(ctx, `
INSERT OR IGNORE INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id,
 current_observation_id, last_complete_generation_id, freshness, updated_at_us)
VALUES (?, ?, ?, NULL, ?, ?, 'fresh', ?)`, projection.pullRequestID, projection.repositoryID,
		generation.Scope.ConnectionID, observationID, completeGeneration, unixMicro(now))
	if err != nil {
		return fmt.Errorf("insert pull request projection: %w", err)
	}
	_, err = conn.ExecContext(ctx, `
UPDATE pull_request_projection_state
SET last_complete_generation_id = CASE WHEN ? THEN ? ELSE last_complete_generation_id END,
    freshness = 'fresh', updated_at_us = MAX(updated_at_us, ?)
WHERE pull_request_id = ?`, complete, generation.ID, unixMicro(now), projection.pullRequestID)
	if err != nil {
		return fmt.Errorf("refresh pull request projection: %w", err)
	}
	_, err = conn.ExecContext(ctx, `
UPDATE pull_request_projection_state
SET current_observation_id = ?, current_revision_id = NULL, updated_at_us = MAX(updated_at_us, ?)
WHERE pull_request_id = ? AND current_observation_id <> ? AND EXISTS (
 SELECT 1 FROM pull_request_observations previous, pull_request_observations candidate
 WHERE previous.id = pull_request_projection_state.current_observation_id AND candidate.id = ?
 AND (candidate.github_updated_at_us > previous.github_updated_at_us
   OR (candidate.github_updated_at_us = previous.github_updated_at_us AND candidate.source_priority > previous.source_priority)
   OR (candidate.github_updated_at_us = previous.github_updated_at_us AND candidate.source_priority = previous.source_priority AND candidate.observed_at_us > previous.observed_at_us)
   OR (candidate.github_updated_at_us = previous.github_updated_at_us AND candidate.source_priority = previous.source_priority AND candidate.observed_at_us = previous.observed_at_us AND candidate.id > previous.id))
)`, observationID, unixMicro(now), projection.pullRequestID, observationID, observationID)
	if err != nil {
		return fmt.Errorf("advance pull request projection: %w", err)
	}
	return nil
}

func closeRelationship(ctx context.Context, conn *sql.Conn, generation reconcile.Generation, closure reconcile.RelationshipClosure, now time.Time) (int, error) {
	var endingObservationID string
	if err := conn.QueryRowContext(ctx, `
SELECT item.observation_id
FROM pr_relationships relationship
JOIN connections connection ON connection.id = relationship.connection_id
JOIN reconciliation_generation_items item
  ON item.generation_id = ? AND item.pull_request_id = relationship.pull_request_id
	JOIN pull_request_projection_state projection
	  ON projection.pull_request_id = relationship.pull_request_id
	 AND projection.current_observation_id = item.observation_id
	JOIN pull_request_observations observation ON observation.id = item.observation_id
WHERE relationship.id = ? AND relationship.connection_id = ?
  AND relationship.relationship_kind = ?
  AND relationship.subject_database_id = connection.account_database_id
	  AND (observation.github_state IN ('closed', 'merged') OR NOT EXISTS (
	    SELECT 1 FROM json_each(observation.relationship_set_json) fact
	    WHERE fact.value = relationship.relationship_kind
	  ))
  AND relationship.active_until_us IS NULL`, generation.ID, closure.RelationshipID,
		generation.Scope.ConnectionID, relationshipKindForScope(generation.Scope.Kind)).Scan(&endingObservationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errors.New("relationship closure requires active in-scope relationship and direct observation")
		}
		return 0, fmt.Errorf("read relationship closure evidence: %w", err)
	}
	result, err := conn.ExecContext(ctx, `
UPDATE pr_relationships
SET active_until_us = ?, ended_by_observation_id = ?, ended_by_generation_id = ?, updated_at_us = ?
WHERE id = ? AND connection_id = ? AND relationship_kind = ?
	AND active_until_us IS NULL`, unixMicro(now), endingObservationID, generation.ID,
		unixMicro(now), closure.RelationshipID, generation.Scope.ConnectionID,
		relationshipKindForScope(generation.Scope.Kind))
	if err != nil {
		return 0, fmt.Errorf("close pull request relationship: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count closed pull request relationship: %w", err)
	}
	if affected != 1 {
		return 0, errors.New("relationship closure does not identify an active relationship in scope")
	}
	return 1, nil
}

func updateCheckpoint(ctx context.Context, conn *sql.Conn, generation reconcile.Generation, state reconcile.GenerationState, now time.Time) error {
	var completeGeneration any
	if state == reconcile.GenerationComplete {
		completeGeneration = generation.ID
	} else {
		if err := conn.QueryRowContext(ctx, `
SELECT last_complete_generation_id FROM reconciliation_checkpoints
WHERE connection_id = ? AND scope_kind = ? AND scope_key = ? COLLATE NOCASE AND query_partition = ?`,
			generation.Scope.ConnectionID, generation.Scope.Kind, generation.Scope.Key, generation.Scope.QueryPartition).Scan(&completeGeneration); errors.Is(err, sql.ErrNoRows) {
			completeGeneration = nil
		} else if err != nil {
			return fmt.Errorf("read reconciliation checkpoint: %w", err)
		}
	}
	_, err := conn.ExecContext(ctx, `
INSERT INTO reconciliation_checkpoints(
 connection_id, scope_kind, scope_key, query_partition,
 last_attempt_generation_id, last_complete_generation_id, updated_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(connection_id, scope_kind, scope_key, query_partition) DO UPDATE SET
 last_attempt_generation_id = excluded.last_attempt_generation_id,
 last_complete_generation_id = excluded.last_complete_generation_id,
 updated_at_us = excluded.updated_at_us`, generation.Scope.ConnectionID, generation.Scope.Kind,
		generation.Scope.Key, generation.Scope.QueryPartition, generation.ID, completeGeneration, unixMicro(now))
	if err != nil {
		return fmt.Errorf("update reconciliation checkpoint: %w", err)
	}
	return nil
}

func verifyRunningGeneration(ctx context.Context, conn *sql.Conn, generation reconcile.Generation) error {
	var connectionID, kind, key, partition, state string
	var number int64
	if err := conn.QueryRowContext(ctx, `
SELECT connection_id, scope_kind, scope_key, query_partition, generation_number, state
FROM reconciliation_generations WHERE id = ?`, generation.ID).Scan(&connectionID, &kind, &key, &partition, &number, &state); err != nil {
		return fmt.Errorf("read reconciliation generation: %w", err)
	}
	if connectionID != generation.Scope.ConnectionID || kind != string(generation.Scope.Kind) ||
		!strings.EqualFold(key, generation.Scope.Key) || partition != generation.Scope.QueryPartition || number != generation.Number {
		return errors.New("reconciliation generation identity mismatch")
	}
	if state != "running" {
		return errors.New("reconciliation generation is already terminal")
	}
	return nil
}

func validateConnection(input reconcile.ConnectionInput) error {
	if input.ID == "" || input.APIBaseURL == "" || input.AccountLogin == "" || input.AccountDatabaseID <= 0 ||
		input.CredentialRefKind == "" || input.CredentialLocator == "" || input.CheckedAt.IsZero() || input.CheckedAt.UnixMicro() < 0 {
		return errors.New("complete verified GitHub connection is required")
	}
	var permissions map[string]any
	if len(input.PermissionsJSON) == 0 || json.Unmarshal(input.PermissionsJSON, &permissions) != nil || permissions == nil {
		return errors.New("GitHub connection permissions must be a JSON object")
	}
	return nil
}

func validateScope(scope reconcile.Scope) error {
	if scope.ConnectionID == "" || scope.Key == "" || scope.QueryPartition == "" ||
		(scope.Kind != reconcile.ScopeReviewRequested && scope.Kind != reconcile.ScopeAuthored) {
		return errors.New("valid reconciliation scope is required")
	}
	return nil
}

func validateApplyInput(input reconcile.ApplyGenerationInput) error {
	if input.Generation.ID == "" || input.Generation.Number <= 0 || validateScope(input.Generation.Scope) != nil ||
		input.FinishedAt.IsZero() || input.FinishedAt.UnixMicro() < 0 || input.PagesReceived < 0 {
		return errors.New("complete reconciliation generation input is required")
	}
	if len(input.Closures) > 0 && input.State != reconcile.GenerationComplete {
		return errors.New("relationship closures require complete generation")
	}
	if (input.PagesExpected != nil && *input.PagesExpected < 0) || (input.ProviderTotal != nil && *input.ProviderTotal < 0) {
		return errors.New("generation coverage counts cannot be negative")
	}
	if input.State == reconcile.GenerationComplete {
		if input.PagesExpected == nil || input.PagesReceived != *input.PagesExpected || input.ProviderTotal == nil ||
			input.ProviderIncomplete || input.ErrorClass != "" || input.ErrorMessage != "" {
			return errors.New("complete generation requires verified page coverage")
		}
	} else {
		if input.State != reconcile.GenerationPartial && input.State != reconcile.GenerationCapped &&
			input.State != reconcile.GenerationRateLimited && input.State != reconcile.GenerationFailed {
			return errors.New("valid terminal generation state is required")
		}
		if len(input.Items) > 0 && input.State == reconcile.GenerationFailed {
			return errors.New("failed generation cannot retain positive items")
		}
	}
	for _, item := range input.Items {
		if err := validateProjectionItem(input.Generation.Scope, item); err != nil {
			return err
		}
	}
	for _, closure := range input.Closures {
		if closure.RelationshipID == "" {
			return errors.New("relationship closure ID is required")
		}
	}
	return nil
}

func validateProjectionItem(scope reconcile.Scope, item reconcile.ProjectionItem) error {
	owner, name, found := strings.Cut(item.Repository.FullName, "/")
	if item.Repository.GitHubID <= 0 || item.Repository.NodeID == "" || !found || owner == "" || name == "" || strings.Contains(name, "/") ||
		item.PullRequest.GitHubID <= 0 || item.PullRequest.Number <= 0 || item.PullRequest.AuthorDatabaseID <= 0 ||
		item.PullRequest.AuthorLogin == "" || item.PullRequest.BaseRef == "" || item.SubjectDatabaseID <= 0 || item.SubjectLogin == "" {
		return errors.New("projection item lacks authoritative identity")
	}
	if !validDigest(item.PullRequest.FactsSHA256) || !validDigest(item.PullRequest.BodySHA256) ||
		!validSHA(item.PullRequest.HeadSHA) || !validSHA(item.PullRequest.BaseSHA) || item.PullRequest.GitHubUpdatedAt.IsZero() || item.PullRequest.GitHubUpdatedAt.UnixMicro() < 0 {
		return errors.New("projection item has invalid hashes or timestamp")
	}
	for _, value := range [][]byte{item.PullRequest.LabelsJSON, item.PullRequest.RequestedReviewersJSON, item.PullRequest.RelationshipSetJSON} {
		var array []any
		if json.Unmarshal(value, &array) != nil || array == nil {
			return errors.New("projection item arrays must be valid JSON")
		}
	}
	wantRelationship := relationshipKindForScope(scope.Kind)
	if (item.RelationshipKind != "" && string(item.RelationshipKind) != wantRelationship) || !strings.EqualFold(item.SubjectLogin, scope.Key) {
		return errors.New("projection relationship does not match reconciliation scope")
	}
	if item.PullRequest.State != "open" && item.PullRequest.State != "closed" && item.PullRequest.State != "merged" {
		return errors.New("projection item has invalid GitHub state")
	}
	if item.ObservationSource != reconcile.ObservationReconciliation && item.ObservationSource != reconcile.ObservationDirectRefresh {
		return errors.New("projection item has invalid observation source")
	}
	return nil
}

func observationPriority(source reconcile.ObservationSource) int {
	if source == reconcile.ObservationDirectRefresh {
		return 30
	}
	return 10
}

func generationCoverage(items []resolvedProjection) string {
	identities := make([]string, 0, len(items))
	for _, item := range items {
		identities = append(identities, item.pullRequestID+"\x00"+item.observationID)
	}
	sort.Strings(identities)
	digest := sha256.New()
	for _, identity := range identities {
		_, _ = digest.Write([]byte(identity))
		_, _ = digest.Write([]byte{'\n'})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func relationshipKindForScope(kind reconcile.ScopeKind) string {
	if kind == reconcile.ScopeAuthored {
		return string(reconcile.RelationshipAuthored)
	}
	return string(reconcile.RelationshipReviewRequested)
}

func validDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validSHA(value string) bool {
	if len(value) != 40 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func unixMicro(value time.Time) int64 { return value.UTC().UnixMicro() }

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
