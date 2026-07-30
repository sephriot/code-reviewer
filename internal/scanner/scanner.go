package scanner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
	gh "github.com/sephriot/code-reviewer/internal/github"
)

type githubAPI interface {
	ListReviewAssignments(ctx context.Context) (gh.AssignmentSnapshot, error)
	GetPRDetails(ctx context.Context, owner, repo string, number int) (*gh.PRSummary, error)
	GetEffectiveReview(ctx context.Context, owner, repo string, number int) (*gh.EffectiveReview, error)
}

type queueCanceller interface {
	CancelSystemRequest(id int64)
}

type Scanner struct {
	cfg       *config.Config
	gh        githubAPI
	db        *db.DB
	canceller queueCanceller
	onNew     func()
}

func New(cfg *config.Config, client *gh.Client, database *db.DB, onNew func()) *Scanner {
	return &Scanner{cfg: cfg, gh: client, db: database, onNew: onNew}
}

func (s *Scanner) SetQueueCanceller(canceller queueCanceller) {
	s.canceller = canceller
}

func (s *Scanner) Scan(ctx context.Context) error {
	log.Println("scan: starting PR scan")
	snapshot, discoveryErr := s.gh.ListReviewAssignments(ctx)
	if discoveryErr != nil {
		log.Printf("scan: assignment snapshot incomplete: %v", discoveryErr)
	}

	candidates := make(map[string]gh.PRSummary)
	assigned := make(map[string]bool)
	for _, pr := range snapshot.PRs {
		key := prKey(pr.Owner, pr.Repo, pr.Number)
		candidates[key] = pr
		assigned[key] = true
	}

	tracked, err := s.db.ListOpenPRs()
	if err != nil {
		return errors.Join(discoveryErr, fmt.Errorf("list tracked open PRs: %w", err))
	}
	for _, pr := range tracked {
		owner, repo, ok := splitRepo(pr.Repo)
		if !ok {
			log.Printf("scan: invalid tracked repository %q", pr.Repo)
			continue
		}
		key := prKey(owner, repo, pr.PRNumber)
		if _, exists := candidates[key]; !exists {
			candidates[key] = gh.PRSummary{
				Owner: owner, Repo: repo, Number: pr.PRNumber,
			}
		}
	}

	var scanErrors []error
	if discoveryErr != nil {
		scanErrors = append(scanErrors, discoveryErr)
	}
	created := 0
	for key, candidate := range candidates {
		result, err := s.reconcilePR(ctx, candidate, assigned[key], snapshot.Complete)
		if err != nil {
			log.Printf("scan: reconcile %s: %v", key, err)
			scanErrors = append(scanErrors, fmt.Errorf("%s: %w", key, err))
			continue
		}
		for _, requestID := range result.CanceledIDs {
			if s.canceller != nil {
				s.canceller.CancelSystemRequest(requestID)
			}
		}
		if result.CreatedRequestID != 0 {
			created++
		}
	}

	if created > 0 && s.onNew != nil {
		s.onNew()
	}
	log.Printf("scan: done, %d new review requests", created)
	return errors.Join(scanErrors...)
}

func (s *Scanner) reconcilePR(
	ctx context.Context,
	candidate gh.PRSummary,
	assignedInSnapshot bool,
	snapshotComplete bool,
) (db.ReconciliationResult, error) {
	details, err := s.gh.GetPRDetails(ctx, candidate.Owner, candidate.Repo, candidate.Number)
	if err != nil {
		return db.ReconciliationResult{}, err
	}
	effective, err := s.gh.GetEffectiveReview(ctx, candidate.Owner, candidate.Repo, candidate.Number)
	if err != nil {
		return db.ReconciliationResult{}, err
	}

	fullName := details.Owner + "/" + details.Repo
	existing, err := s.db.GetPRByRepoAndNumber(fullName, details.Number)
	if err != nil {
		return db.ReconciliationResult{}, err
	}

	var existingAssigned bool
	var completed bool
	var requests []db.ReviewRequest
	if existing != nil {
		existingAssigned = existing.IsAssigned
		completed, err = s.db.HasCompletedReviewForSHA(existing.ID, details.CommitSHA)
		if err != nil {
			return db.ReconciliationResult{}, err
		}
		requests, err = s.db.ListReviewRequestsForPR(existing.ID)
		if err != nil {
			return db.ReconciliationResult{}, err
		}
	}

	input := ReconcileInput{
		AssignedInSnapshot:      assignedInSnapshot,
		SnapshotComplete:        snapshotComplete,
		ExistingAssigned:        existingAssigned,
		State:                   openOr(details.State),
		HeadSHA:                 details.CommitSHA,
		Draft:                   details.Draft,
		FilteredReason:          s.filterReason(*details),
		HasCompletedLocalReview: completed,
		Requests:                requestFacts(requests),
	}
	if effective != nil {
		input.EffectiveReview = &EffectiveReview{
			ID:    effective.ID,
			State: EffectiveReviewState(effective.State),
		}
	}
	decision := DecideReconciliation(input)

	pr := db.PullRequest{
		Repo:           fullName,
		PRNumber:       details.Number,
		Title:          details.Title,
		Author:         details.Author,
		CommitSHA:      details.CommitSHA,
		Draft:          details.Draft,
		State:          openOr(details.State),
		FilteredReason: decision.FilteredReason,
		GhUpdatedAt:    details.UpdatedAt,
		IsAssigned:     decision.IsAssigned,
	}
	if effective != nil {
		id := effective.ID
		pr.EffectiveReviewID = &id
		pr.EffectiveReviewState = string(effective.State)
	}

	change := db.ReconciliationChange{
		PR:        pr,
		CreateSHA: decision.CreateSHA,
	}
	for _, cancellation := range decision.Cancel {
		change.Cancel = append(change.Cancel, db.RequestStatusChange{
			ID:     cancellation.ID,
			Status: cancellation.Status,
		})
	}
	return s.db.ApplyReconciliation(change)
}

func (s *Scanner) filterReason(pr gh.PRSummary) string {
	if !matchesFilter(pr.Owner+"/"+pr.Repo, s.cfg.RepoFilterRegex()) {
		return "repo"
	}
	if !matchesFilter(pr.Author, s.cfg.AuthorFilterRegex()) {
		return "author"
	}
	return ""
}

func requestFacts(requests []db.ReviewRequest) []RequestFact {
	facts := make([]RequestFact, 0, len(requests))
	for _, request := range requests {
		facts = append(facts, RequestFact{
			ID:        request.ID,
			CommitSHA: request.CommitSHA,
			Status:    request.Status,
		})
	}
	return facts
}

func matchesFilter(value string, patterns []*regexp.Regexp) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

func prKey(owner, repo string, number int) string {
	return strings.ToLower(fmt.Sprintf("%s/%s#%d", owner, repo, number))
}

func splitRepo(fullName string) (string, string, bool) {
	parts := strings.SplitN(fullName, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func openOr(state string) string {
	if state == "" {
		return db.PRStateOpen
	}
	return state
}
