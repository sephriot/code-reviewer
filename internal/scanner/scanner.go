package scanner

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"

	gh "github.com/sephriot/code-reviewer/internal/github"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
)

type Scanner struct {
	cfg   *config.Config
	gh    *gh.Client
	db    *db.DB
	onNew func()
}

func New(cfg *config.Config, gh *gh.Client, d *db.DB, onNew func()) *Scanner {
	return &Scanner{cfg: cfg, gh: gh, db: d, onNew: onNew}
}

func (s *Scanner) Scan(ctx context.Context) error {
	log.Println("scan: starting PR scan")

	var allPRs []gh.PRSummary

	assigned, err := s.gh.ListAssignedPRs(ctx)
	if err != nil {
		return fmt.Errorf("list assigned PRs: %w", err)
	}
	allPRs = append(allPRs, assigned...)

	if s.cfg.OwnPRMode != "off" {
		own, err := s.gh.ListOwnPRs(ctx)
		if err != nil {
			return fmt.Errorf("list own PRs: %w", err)
		}
		allPRs = append(allPRs, own...)
	}

	log.Printf("scan: found %d PRs total", len(allPRs))

	dedup := map[string]gh.PRSummary{}
	for _, pr := range allPRs {
		key := prKey(pr.Owner, pr.Repo, pr.Number)
		dedup[key] = pr
	}

	newRequests := 0
	for _, pr := range dedup {
		created, err := s.processPR(ctx, pr)
		if err != nil {
			log.Printf("scan: error processing %s/%s#%d: %v", pr.Owner, pr.Repo, pr.Number, err)
			continue
		}
		if created {
			newRequests++
		}
	}

	log.Printf("scan: done, %d new review requests", newRequests)

	if newRequests > 0 && s.onNew != nil {
		s.onNew()
	}
	return nil
}

func (s *Scanner) processPR(ctx context.Context, pr gh.PRSummary) (bool, error) {
	fullName := pr.Owner + "/" + pr.Repo

	if !matchesFilter(fullName, s.cfg.RepoFilterRegex()) {
		log.Printf("scan: filtered out repo=%s %s/%s#%d", fullName, pr.Owner, pr.Repo, pr.Number)
		return false, nil
	}
	if !matchesFilter(pr.Author, s.cfg.AuthorFilterRegex()) {
		log.Printf("scan: filtered out author=%s %s/%s#%d", pr.Author, pr.Owner, pr.Repo, pr.Number)
		return false, nil
	}

	details, err := s.gh.GetPRDetails(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return false, err
	}

	if details.Draft {
		log.Printf("scan: skip draft %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
		return false, nil
	}

	existing, err := s.db.GetPRByRepoAndNumber(fullName, pr.Number)
	if err != nil {
		return false, err
	}

	if existing != nil && existing.CommitSHA == details.CommitSHA {
		return false, nil
	}

	if existing != nil && existing.CommitSHA != details.CommitSHA {
		if err := s.db.MarkPROutdated(existing.ID); err != nil {
			return false, err
		}
		log.Printf("scan: new commit on %s/%s#%d, marked outdated", pr.Owner, pr.Repo, pr.Number)
	}

	hasReviewed, err := s.gh.HasUserReviewed(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return false, err
	}

	needsReview := !hasReviewed
	prID, err := s.db.UpsertPR(db.PullRequest{
		Repo:        fullName,
		PRNumber:    pr.Number,
		Title:       details.Title,
		Author:      details.Author,
		CommitSHA:   details.CommitSHA,
		Draft:       details.Draft,
		State:       "open",
		NeedsReview: needsReview,
	})
	if err != nil {
		return false, err
	}

	if !needsReview {
		return false, nil
	}

	_, err = s.db.CreateReviewRequest(prID)
	if err != nil {
		return false, err
	}
	log.Printf("scan: review request created for %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
	return true, nil
}

func matchesFilter(value string, patterns []*regexp.Regexp) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, re := range patterns {
		if re.MatchString(value) {
			return true
		}
	}
	return false
}

func prKey(owner, repo string, number int) string {
	return strings.ToLower(fmt.Sprintf("%s/%s#%d", owner, repo, number))
}
