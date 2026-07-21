package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	githubadapter "github.com/sephriot/code-reviewer/internal/adapters/github"
)

// Config identifies one durable local-user connection without carrying its secret.
type Config struct {
	ConnectionID      string
	APIBaseURL        string
	CredentialRefKind string
	CredentialLocator string
}

// Service performs one-shot, read-only GitHub reconciliation.
type Service struct {
	reader githubadapter.Reader
	store  Store
	now    func() time.Time
}

func NewService(reader githubadapter.Reader, store Store) (*Service, error) {
	if reader == nil || store == nil {
		return nil, errors.New("GitHub reader and projection store are required")
	}
	return &Service{reader: reader, store: store, now: func() time.Time { return time.Now().UTC() }}, nil
}

// Reconcile projects assigned and authored pull requests without scheduling work.
func (s *Service) Reconcile(ctx context.Context, config Config) (Report, error) {
	if config.ConnectionID == "" || config.APIBaseURL == "" || config.CredentialRefKind == "" || config.CredentialLocator == "" {
		return Report{}, errors.New("connection ID, API URL, and credential reference are required")
	}
	authenticated, err := s.reader.AuthenticatedUser(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("authenticate GitHub reader: %w", err)
	}
	if err := s.store.UpsertGitHubConnection(ctx, ConnectionInput{
		ID: config.ConnectionID, APIBaseURL: config.APIBaseURL,
		AccountLogin: authenticated.User.Login, AccountNodeID: authenticated.User.NodeID,
		AccountDatabaseID: authenticated.User.ID, CredentialRefKind: config.CredentialRefKind,
		CredentialLocator: config.CredentialLocator,
		PermissionsJSON:   []byte(`{"pull_requests":"read"}`), CheckedAt: s.now(),
	}); err != nil {
		return Report{}, fmt.Errorf("store GitHub connection: %w", err)
	}

	report := Report{ConnectionID: config.ConnectionID, AccountLogin: authenticated.User.Login}
	specifications := []scopeSpecification{
		{kind: ScopeReviewRequested, relationship: RelationshipReviewRequested, query: "is:pr state:open review-requested:" + authenticated.User.Login},
		{kind: ScopeAuthored, relationship: RelationshipAuthored, query: "is:pr state:open author:" + authenticated.User.Login},
	}
	for _, specification := range specifications {
		scopeReport, err := s.reconcileScope(ctx, config.ConnectionID, authenticated.User, specification)
		if err != nil {
			return Report{}, err
		}
		report.Scopes = append(report.Scopes, scopeReport)
	}
	return report, nil
}

type scopeSpecification struct {
	kind         ScopeKind
	relationship RelationshipKind
	query        string
}

type candidate struct {
	owner, repository string
	number            int
}

func (s *Service) reconcileScope(ctx context.Context, connectionID string, account githubadapter.User, specification scopeSpecification) (ScopeReport, error) {
	scope := Scope{ConnectionID: connectionID, Kind: specification.kind, Key: account.Login, QueryPartition: "all"}
	generation, err := s.store.NextReconciliationGeneration(ctx, scope, s.now())
	if err != nil {
		return ScopeReport{}, fmt.Errorf("start %s reconciliation: %w", specification.kind, err)
	}
	report := ScopeReport{Scope: specification.kind, GenerationID: generation.ID, GenerationNumber: generation.Number}
	active, err := s.store.ListActiveRelationships(ctx, scope)
	if err != nil {
		return s.finishFailedScope(ctx, generation, report, "active_relationships_failed", err)
	}

	state := GenerationComplete
	var errorClass, errorMessage string
	var providerTotal *int
	providerIncomplete := false
	candidates := make(map[string]candidate)
	pageNumber := 1
	for {
		page, searchErr := s.reader.SearchPullRequests(ctx, specification.query, pageNumber)
		if searchErr != nil {
			state = classifyFailure(state, searchErr, report.PagesReceived == 0 && len(candidates) == 0)
			errorClass, errorMessage = classifyError("search_failed", searchErr)
			break
		}
		report.PagesReceived++
		if providerTotal == nil {
			total := page.TotalCount
			providerTotal = &total
			report.ProviderTotal = total
		} else if page.TotalCount != *providerTotal && state == GenerationComplete {
			state = GenerationPartial
			errorClass, errorMessage = "provider_total_changed", "GitHub search total changed during pagination"
		}
		if page.IncompleteResults {
			providerIncomplete = true
			state = worsenState(state, GenerationPartial)
			errorClass, errorMessage = "provider_incomplete", "GitHub marked search results incomplete"
		}
		if page.TotalCount > 1000 {
			state = worsenState(state, GenerationCapped)
			errorClass, errorMessage = "provider_capped", "GitHub search exceeds the 1000-result window"
		}
		for _, item := range page.Candidates {
			key := candidateKey(item.Owner, item.Repository, item.Number)
			candidates[key] = candidate{owner: item.Owner, repository: item.Repository, number: item.Number}
		}
		if page.NextPage == 0 {
			break
		}
		pageNumber = page.NextPage
	}
	report.Candidates = len(candidates)
	if state == GenerationComplete && providerTotal != nil && len(candidates) != *providerTotal {
		state = GenerationPartial
		errorClass, errorMessage = "provider_coverage_mismatch", "GitHub search result count did not match its reported total"
	}

	keys := make([]string, 0, len(candidates))
	for key := range candidates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	items := make([]ProjectionItem, 0, len(keys)+len(active))
	positive := make(map[string]struct{})
	for _, key := range keys {
		item := candidates[key]
		result, detailErr := s.reader.GetPullRequest(ctx, item.owner, item.repository, item.number, "")
		if detailErr != nil {
			report.DetailFailures++
			state = worsenState(state, classifyFailure(GenerationPartial, detailErr, false))
			if errorClass == "" || state == GenerationRateLimited {
				errorClass, errorMessage = classifyError("detail_failed", detailErr)
			}
			continue
		}
		if result.PullRequest == nil {
			report.DetailFailures++
			state = worsenState(state, GenerationPartial)
			errorClass, errorMessage = "detail_missing", "GitHub returned no pull-request representation"
			continue
		}
		projected, present, projectErr := projectionItem(*result.PullRequest, account, specification.relationship, ObservationReconciliation)
		if projectErr != nil {
			return ScopeReport{}, projectErr
		}
		if !present {
			report.SearchGaps++
			state = worsenState(state, GenerationPartial)
			errorClass, errorMessage = "search_index_gap", "search result no longer has the requested relationship"
		}
		items = append(items, projected)
		if present {
			positive[candidateKey(item.owner, item.repository, item.number)] = struct{}{}
		}
	}

	closures := make([]RelationshipClosure, 0)
	if state == GenerationComplete {
		for _, relationship := range active {
			key := candidateKey(relationship.RepositoryOwner, relationship.RepositoryName, relationship.PullRequestNumber)
			if _, exists := positive[key]; exists {
				continue
			}
			result, detailErr := s.reader.GetPullRequest(ctx, relationship.RepositoryOwner, relationship.RepositoryName, relationship.PullRequestNumber, "")
			if detailErr != nil || result.PullRequest == nil {
				report.DetailFailures++
				state = worsenState(state, classifyFailure(GenerationPartial, detailErr, false))
				errorClass, errorMessage = classifyError("direct_refresh_failed", detailErr)
				continue
			}
			projected, present, projectErr := projectionItem(*result.PullRequest, account, specification.relationship, ObservationDirectRefresh)
			if projectErr != nil {
				return ScopeReport{}, projectErr
			}
			items = append(items, projected)
			if result.PullRequest.UpdatedAt.Before(relationship.CurrentGitHubUpdatedAt) {
				report.StaleDetails++
				state = worsenState(state, GenerationPartial)
				errorClass, errorMessage = "stale_direct_refresh", "direct refresh was older than the selected local observation"
				continue
			}
			if present && result.PullRequest.State == "open" && !result.PullRequest.Merged {
				report.SearchGaps++
				state = worsenState(state, GenerationPartial)
				errorClass, errorMessage = "search_index_gap", "direct refresh retained a relationship absent from search"
				continue
			}
			closures = append(closures, RelationshipClosure{RelationshipID: relationship.ID})
		}
	}
	if state != GenerationComplete {
		closures = nil
	}

	deduplicatedItems := deduplicateProjectionItems(items)
	report.Projected = len(deduplicatedItems)
	input := ApplyGenerationInput{
		Generation: generation, State: state, PagesReceived: report.PagesReceived,
		ProviderTotal: providerTotal, ProviderIncomplete: providerIncomplete,
		ErrorClass: errorClass, ErrorMessage: errorMessage, FinishedAt: s.now(),
		Items: deduplicatedItems, Closures: closures,
	}
	if state == GenerationComplete {
		expected := report.PagesReceived
		input.PagesExpected = &expected
	}
	persisted, err := s.store.ApplyReconciliationGeneration(ctx, input)
	if err != nil {
		return ScopeReport{}, fmt.Errorf("apply %s reconciliation: %w", specification.kind, err)
	}
	report.State = state
	report.Persistence = persisted
	return report, nil
}

func (s *Service) finishFailedScope(ctx context.Context, generation Generation, report ScopeReport, class string, failure error) (ScopeReport, error) {
	_, applyErr := s.store.ApplyReconciliationGeneration(ctx, ApplyGenerationInput{
		Generation: generation, State: GenerationFailed, ErrorClass: class,
		ErrorMessage: safeError(failure), FinishedAt: s.now(),
	})
	if applyErr != nil {
		return ScopeReport{}, fmt.Errorf("record failed reconciliation after %v: %w", failure, applyErr)
	}
	report.State = GenerationFailed
	return report, nil
}

func projectionItem(pullRequest githubadapter.PullRequest, account githubadapter.User, relationship RelationshipKind, source ObservationSource) (ProjectionItem, bool, error) {
	present := relationshipPresent(pullRequest, account.ID, relationship)
	relationships := make([]string, 0, 2)
	if pullRequest.Author.ID == account.ID {
		relationships = append(relationships, string(RelationshipAuthored))
	}
	for _, reviewer := range pullRequest.RequestedReviewers {
		if reviewer.ID == account.ID {
			relationships = append(relationships, string(RelationshipReviewRequested))
			break
		}
	}
	sort.Strings(relationships)
	reviewers := make([]githubadapter.User, len(pullRequest.RequestedReviewers))
	copy(reviewers, pullRequest.RequestedReviewers)
	sort.Slice(reviewers, func(i, j int) bool { return reviewers[i].ID < reviewers[j].ID })
	labels := make([]string, len(pullRequest.Labels))
	copy(labels, pullRequest.Labels)
	sort.Strings(labels)
	reviewerJSON, err := json.Marshal(reviewers)
	if err != nil {
		return ProjectionItem{}, false, fmt.Errorf("encode requested reviewers: %w", err)
	}
	labelsJSON, err := json.Marshal(labels)
	if err != nil {
		return ProjectionItem{}, false, fmt.Errorf("encode labels: %w", err)
	}
	relationshipJSON, err := json.Marshal(relationships)
	if err != nil {
		return ProjectionItem{}, false, fmt.Errorf("encode relationships: %w", err)
	}
	bodyDigest := sha256.Sum256([]byte(pullRequest.Body))
	state := pullRequest.State
	if pullRequest.Merged {
		state = "merged"
	}
	facts := struct {
		Version            int                  `json:"version"`
		RepositoryID       int64                `json:"repository_id"`
		PullRequestID      int64                `json:"pull_request_id"`
		Number             int                  `json:"number"`
		Title              string               `json:"title"`
		BodySHA256         string               `json:"body_sha256"`
		Author             githubadapter.User   `json:"author"`
		State              string               `json:"state"`
		Draft              bool                 `json:"draft"`
		HeadSHA            string               `json:"head_sha"`
		BaseSHA            string               `json:"base_sha"`
		BaseRef            string               `json:"base_ref"`
		Labels             []string             `json:"labels"`
		RequestedReviewers []githubadapter.User `json:"requested_reviewers"`
		UpdatedAt          string               `json:"updated_at"`
	}{
		Version: 1, RepositoryID: pullRequest.TargetRepository.ID,
		PullRequestID: pullRequest.ID, Number: pullRequest.Number, Title: pullRequest.Title,
		BodySHA256: hex.EncodeToString(bodyDigest[:]), Author: pullRequest.Author,
		State: state, Draft: pullRequest.Draft, HeadSHA: pullRequest.HeadSHA,
		BaseSHA: pullRequest.BaseSHA, BaseRef: pullRequest.BaseRef, Labels: labels,
		RequestedReviewers: reviewers, UpdatedAt: pullRequest.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return ProjectionItem{}, false, fmt.Errorf("encode observation facts: %w", err)
	}
	factsDigest := sha256.Sum256(factsJSON)
	projectedRelationship := RelationshipKind("")
	if present {
		projectedRelationship = relationship
	}
	return ProjectionItem{
		Repository: RepositoryFacts{GitHubID: pullRequest.TargetRepository.ID, NodeID: pullRequest.TargetRepository.NodeID, FullName: pullRequest.TargetRepository.FullName},
		PullRequest: PullRequestFacts{
			GitHubID: pullRequest.ID, NodeID: pullRequest.NodeID, Number: pullRequest.Number,
			Title: pullRequest.Title, AuthorLogin: pullRequest.Author.Login,
			AuthorDatabaseID: pullRequest.Author.ID, URL: pullRequest.URL, State: state,
			IsDraft: pullRequest.Draft, HeadSHA: pullRequest.HeadSHA, BaseSHA: pullRequest.BaseSHA,
			BaseRef: pullRequest.BaseRef, BodySHA256: hex.EncodeToString(bodyDigest[:]),
			LabelsJSON: labelsJSON, RequestedReviewersJSON: reviewerJSON,
			RelationshipSetJSON: relationshipJSON, FactsSHA256: hex.EncodeToString(factsDigest[:]),
			GitHubUpdatedAt: pullRequest.UpdatedAt,
		},
		RelationshipKind: projectedRelationship, SubjectLogin: account.Login,
		SubjectDatabaseID: account.ID, ObservationSource: source,
	}, present, nil
}

func relationshipPresent(pullRequest githubadapter.PullRequest, accountID int64, relationship RelationshipKind) bool {
	if pullRequest.State != "open" || pullRequest.Merged {
		return false
	}
	switch relationship {
	case RelationshipAuthored:
		return pullRequest.Author.ID == accountID
	case RelationshipReviewRequested:
		for _, reviewer := range pullRequest.RequestedReviewers {
			if reviewer.ID == accountID {
				return true
			}
		}
	}
	return false
}

func candidateKey(owner, repository string, number int) string {
	return strings.ToLower(owner+"/"+repository) + "#" + strconv.Itoa(number)
}

func deduplicateProjectionItems(items []ProjectionItem) []ProjectionItem {
	byPR := make(map[string]ProjectionItem, len(items))
	for _, item := range items {
		key := strconv.FormatInt(item.Repository.GitHubID, 10) + "#" + strconv.Itoa(item.PullRequest.Number)
		existing, found := byPR[key]
		if !found || (existing.RelationshipKind == "" && item.RelationshipKind != "") {
			byPR[key] = item
		}
	}
	keys := make([]string, 0, len(byPR))
	for key := range byPR {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]ProjectionItem, 0, len(keys))
	for _, key := range keys {
		result = append(result, byPR[key])
	}
	return result
}

func worsenState(current, candidate GenerationState) GenerationState {
	severity := map[GenerationState]int{
		GenerationComplete: 0, GenerationPartial: 1, GenerationCapped: 2,
		GenerationRateLimited: 3, GenerationFailed: 4,
	}
	if severity[candidate] > severity[current] {
		return candidate
	}
	return current
}

func classifyFailure(current GenerationState, err error, empty bool) GenerationState {
	var httpError *githubadapter.HTTPError
	if errors.As(err, &httpError) && (httpError.StatusCode == 429 || httpError.StatusCode == 403) {
		return GenerationRateLimited
	}
	if empty {
		return GenerationFailed
	}
	return worsenState(current, GenerationPartial)
}

func classifyError(fallback string, err error) (string, string) {
	if err == nil {
		return fallback, fallback
	}
	var httpError *githubadapter.HTTPError
	if errors.As(err, &httpError) && (httpError.StatusCode == 429 || httpError.StatusCode == 403) {
		return "rate_limited", safeError(err)
	}
	return fallback, safeError(err)
}

func safeError(err error) string {
	if err == nil {
		return "unknown reconciliation failure"
	}
	message := strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, err.Error())
	message = strings.Join(strings.Fields(message), " ")
	runes := []rune(message)
	if len(runes) > 512 {
		message = string(runes[:512])
	}
	return message
}
