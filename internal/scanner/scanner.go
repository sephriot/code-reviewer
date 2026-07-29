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

// githubAPI is the subset of GitHub client methods the scanner needs.
type githubAPI interface {
	ListAssignedPRs(ctx context.Context) ([]gh.PRSummary, error)
	ListOwnPRs(ctx context.Context) ([]gh.PRSummary, error)
	GetPRDetails(ctx context.Context, owner, repo string, number int) (*gh.PRSummary, error)
	HasUserReviewed(ctx context.Context, owner, repo string, number int) (bool, error)
}

type Scanner struct {
	cfg   *config.Config
	gh    githubAPI
	db    *db.DB
	onNew func()
}

func New(cfg *config.Config, client *gh.Client, d *db.DB, onNew func()) *Scanner {
	return &Scanner{cfg: cfg, gh: client, db: d, onNew: onNew}
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

	s.reconcileStalePRs(ctx, dedup)
	s.backfillMergedStates(ctx)

	log.Printf("scan: done, %d new review requests", newRequests)

	if newRequests > 0 && s.onNew != nil {
		s.onNew()
	}
	return nil
}


// ensureExternalReview records reviewed_externally when GitHub already has our review.
func (s *Scanner) ensureExternalReview(ctx context.Context, owner, repo string, number int, prID int64, commitSHA string) {
	hasReviewed, err := s.gh.HasUserReviewed(ctx, owner, repo, number)
	if err != nil {
		log.Printf("scan: error checking review status for %s/%s#%d: %v", owner, repo, number, err)
		return
	}
	if !hasReviewed {
		return
	}
	s.recordExternalReview(owner, repo, number, prID, commitSHA)
}

func (s *Scanner) recordExternalReview(owner, repo string, number int, prID int64, commitSHA string) {
	extExists, err := s.db.HasExternalReview(prID, commitSHA)
	if err != nil {
		log.Printf("scan: error checking external review for %s/%s#%d: %v", owner, repo, number, err)
		return
	}
	if extExists {
		return
	}
	if _, err := s.db.CreateExternalReview(prID, commitSHA); err != nil {
		log.Printf("scan: error recording external review for %s/%s#%d: %v", owner, repo, number, err)
		return
	}
	log.Printf("scan: recorded external review for %s/%s#%d", owner, repo, number)
}

func (s *Scanner) processPR(ctx context.Context, pr gh.PRSummary) (bool, error) {
	fullName := pr.Owner + "/" + pr.Repo

	if !matchesFilter(fullName, s.cfg.RepoFilterRegex()) {
		log.Printf("scan: filtered out repo=%s %s/%s#%d", fullName, pr.Owner, pr.Repo, pr.Number)
		prID, err := s.db.UpsertPR(db.PullRequest{
			Repo:           fullName,
			PRNumber:       pr.Number,
			Title:          pr.Title,
			Author:         pr.Author,
			CommitSHA:      pr.CommitSHA,
			Draft:          pr.Draft,
			State:          openOr(pr.State),
			NeedsReview:    false,
			FilteredReason: "repo",
		})
		if err != nil {
			return false, err
		}
		s.ensureExternalReview(ctx, pr.Owner, pr.Repo, pr.Number, prID, pr.CommitSHA)
		return false, nil
	}
	if !matchesFilter(pr.Author, s.cfg.AuthorFilterRegex()) {
		log.Printf("scan: filtered out author=%s %s/%s#%d", pr.Author, pr.Owner, pr.Repo, pr.Number)
		prID, err := s.db.UpsertPR(db.PullRequest{
			Repo:           fullName,
			PRNumber:       pr.Number,
			Title:          pr.Title,
			Author:         pr.Author,
			CommitSHA:      pr.CommitSHA,
			Draft:          pr.Draft,
			State:          openOr(pr.State),
			NeedsReview:    false,
			FilteredReason: "author",
		})
		if err != nil {
			return false, err
		}
		s.ensureExternalReview(ctx, pr.Owner, pr.Repo, pr.Number, prID, pr.CommitSHA)
		return false, nil
	}

	details, err := s.gh.GetPRDetails(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return false, err
	}

	if details.Draft {
		log.Printf("scan: filtered out draft %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
		prID, err := s.db.UpsertPR(db.PullRequest{
			Repo:           fullName,
			PRNumber:       pr.Number,
			Title:          details.Title,
			Author:         details.Author,
			CommitSHA:      details.CommitSHA,
			Draft:          true,
			State:          "open",
			NeedsReview:    false,
			FilteredReason: "draft",
		})
		if err != nil {
			return false, err
		}
		s.ensureExternalReview(ctx, pr.Owner, pr.Repo, pr.Number, prID, details.CommitSHA)
		return false, nil
	}

	if details.State != "open" {
		log.Printf("scan: closed/merged %s/%s#%d state=%s -> history", pr.Owner, pr.Repo, pr.Number, details.State)
		prID, err := s.db.UpsertPR(db.PullRequest{
			Repo:           fullName,
			PRNumber:       pr.Number,
			Title:          details.Title,
			Author:         details.Author,
			CommitSHA:      details.CommitSHA,
			Draft:          details.Draft,
			State:          details.State,
			NeedsReview:    false,
			FilteredReason: "",
		})
		if err != nil {
			return false, err
		}
		s.ensureExternalReview(ctx, pr.Owner, pr.Repo, pr.Number, prID, details.CommitSHA)
		return false, nil
	}

	existing, err := s.db.GetPRByRepoAndNumber(fullName, pr.Number)
	if err != nil {
		return false, err
	}

	if existing != nil && existing.CommitSHA == details.CommitSHA {
		hasReviewed, err := s.gh.HasUserReviewed(ctx, pr.Owner, pr.Repo, pr.Number)
		if err != nil {
			return false, err
		}
		needsReview := !hasReviewed
		// Clear any prior filter reason (e.g. draft→ready same SHA) and sync needs_review.
		_, err = s.db.UpsertPR(db.PullRequest{
			Repo:           fullName,
			PRNumber:       pr.Number,
			Title:          details.Title,
			Author:         details.Author,
			CommitSHA:      details.CommitSHA,
			Draft:          false,
			State:          "open",
			NeedsReview:    needsReview,
			IsOutdated:     existing.IsOutdated,
			FilteredReason: "",
		})
		if err != nil {
			return false, err
		}
		if hasReviewed {
			log.Printf("scan: %s/%s#%d already reviewed, needs_review=false", pr.Owner, pr.Repo, pr.Number)
			s.recordExternalReview(pr.Owner, pr.Repo, pr.Number, existing.ID, details.CommitSHA)
		} else {
			log.Printf("scan: %s/%s#%d not reviewed, needs_review=true", pr.Owner, pr.Repo, pr.Number)
		}
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
		Repo:           fullName,
		PRNumber:       pr.Number,
		Title:          details.Title,
		Author:         details.Author,
		CommitSHA:      details.CommitSHA,
		Draft:          details.Draft,
		State:          "open",
		NeedsReview:    needsReview,
		FilteredReason: "",
	})
	if err != nil {
		return false, err
	}

	if !needsReview {
		s.recordExternalReview(pr.Owner, pr.Repo, pr.Number, prID, details.CommitSHA)
		return false, nil
	}

	_, err = s.db.CreateReviewRequest(prID)
	if err != nil {
		return false, err
	}
	log.Printf("scan: review request created for %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
	return true, nil
}

func (s *Scanner) reconcileStalePRs(ctx context.Context, seen map[string]gh.PRSummary) {
	// Include filtered opens so a draft/repo/author discard that merges leaves Filtered.
	openPRs, err := s.db.ListOpenPRs()
	if err != nil {
		log.Printf("scan: error listing open PRs for reconciliation: %v", err)
		return
	}

	for _, pr := range openPRs {
		parts := strings.SplitN(pr.Repo, "/", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.ToLower(fmt.Sprintf("%s/%s#%d", parts[0], parts[1], pr.PRNumber))
		if _, found := seen[key]; found {
			continue
		}

		details, err := s.gh.GetPRDetails(ctx, parts[0], parts[1], pr.PRNumber)
		if err != nil {
			log.Printf("scan: reconcile error for %s#%d: %v", pr.Repo, pr.PRNumber, err)
			continue
		}
		if details.State == "open" {
			continue
		}

		log.Printf("scan: reconcile %s#%d state %s -> %s (history)", pr.Repo, pr.PRNumber, pr.State, details.State)
		prID, err := s.db.UpsertPR(db.PullRequest{
			Repo:           pr.Repo,
			PRNumber:       pr.PRNumber,
			Title:          details.Title,
			Author:         details.Author,
			CommitSHA:      details.CommitSHA,
			Draft:          details.Draft,
			State:          details.State,
			NeedsReview:    false,
			FilteredReason: "",
		})
		if err != nil {
			log.Printf("scan: reconcile upsert error for %s#%d: %v", pr.Repo, pr.PRNumber, err)
			continue
		}
		s.ensureExternalReview(ctx, parts[0], parts[1], pr.PRNumber, prID, details.CommitSHA)
	}
}

// backfillMergedStates re-fetches PRs stored as closed and upgrades them to
// merged when GitHub reports merged. Needed because older scans stored
// GitHub's raw state (closed) for merged PRs.
func (s *Scanner) backfillMergedStates(ctx context.Context) {
	closed, err := s.db.ListClosedPRs()
	if err != nil {
		log.Printf("scan: error listing closed PRs for merged backfill: %v", err)
		return
	}
	updated := 0
	for _, pr := range closed {
		parts := strings.SplitN(pr.Repo, "/", 2)
		if len(parts) != 2 {
			continue
		}
		details, err := s.gh.GetPRDetails(ctx, parts[0], parts[1], pr.PRNumber)
		if err != nil {
			log.Printf("scan: merged backfill error for %s#%d: %v", pr.Repo, pr.PRNumber, err)
			continue
		}
		if details.State != db.PRStateMerged {
			continue
		}
		log.Printf("scan: backfill %s#%d closed -> merged", pr.Repo, pr.PRNumber)
		_, err = s.db.UpsertPR(db.PullRequest{
			Repo:           pr.Repo,
			PRNumber:       pr.PRNumber,
			Title:          details.Title,
			Author:         details.Author,
			CommitSHA:      details.CommitSHA,
			Draft:          details.Draft,
			State:          db.PRStateMerged,
			NeedsReview:    false,
			FilteredReason: "",
		})
		if err != nil {
			log.Printf("scan: merged backfill upsert error for %s#%d: %v", pr.Repo, pr.PRNumber, err)
			continue
		}
		updated++
	}
	if updated > 0 {
		log.Printf("scan: backfilled %d closed PRs to merged", updated)
	}
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

func openOr(state string) string {
	if state == "" {
		return "open"
	}
	return state
}
