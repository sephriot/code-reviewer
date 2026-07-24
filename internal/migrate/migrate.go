package migrate

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/db"
	_ "modernc.org/sqlite"
)

// Stats summarizes a migration run.
type Stats struct {
	Candidates       int
	PRsCreated       int
	ReviewsInserted  int
	CommentsInserted int
	SkippedExisting  int
	SkippedPending   int
}

type inlineComment struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

type candidate struct {
	Repo           string
	PRNumber       int
	Title          string
	Author         string
	CommitSHA      string
	Outcome        string
	Summary        string
	GeneralComment string
	Published      bool
	Closed         bool
	CreatedAt      string // UTC "2006-01-02 15:04:05"
	Comments       []inlineComment
}

// MapActionToOutcome maps legacy v1 review_action values to v2 outcomes.
func MapActionToOutcome(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "approve_without_comment":
		return db.ReviewOutcomeApproveWithoutComments
	case "approve_with_comment":
		return db.ReviewOutcomeApproveWithComments
	case "request_changes":
		return db.ReviewOutcomeChangesRequested
	case "requires_human_review":
		return db.ReviewOutcomeHumanReview
	default:
		return db.ReviewOutcomeToolFailed
	}
}

// Run migrates pr_reviews + pending_approvals from legacyPath into targetPath.
// dryRun collects candidates but does not write.
func Run(legacyPath, targetPath string, dryRun bool) (Stats, error) {
	var stats Stats

	legacy, err := sql.Open("sqlite", legacyPath+"?mode=ro")
	if err != nil {
		return stats, fmt.Errorf("open legacy: %w", err)
	}
	defer legacy.Close()

	cands, skippedPending, err := loadCandidates(legacy)
	if err != nil {
		return stats, err
	}
	stats.Candidates = len(cands)
	stats.SkippedPending = skippedPending

	if dryRun {
		return stats, nil
	}

	target, err := db.Open(targetPath)
	if err != nil {
		return stats, fmt.Errorf("open target: %w", err)
	}
	defer target.Close()

	for _, c := range cands {
		prID, created, err := ensurePR(target, c)
		if err != nil {
			return stats, fmt.Errorf("ensure PR %s#%d: %w", c.Repo, c.PRNumber, err)
		}
		if created {
			stats.PRsCreated++
		}

		sha := effectiveSHA(c)
		exists, err := hasReviewForSHA(target, prID, sha)
		if err != nil {
			return stats, err
		}
		if exists {
			stats.SkippedExisting++
			continue
		}

		c.CommitSHA = sha
		reviewID, err := insertHistoricalReview(target, prID, c)
		if err != nil {
			return stats, fmt.Errorf("insert review %s#%d@%s: %w", c.Repo, c.PRNumber, c.CommitSHA, err)
		}
		stats.ReviewsInserted++

		for _, comment := range c.Comments {
			if comment.File == "" && comment.Message == "" {
				continue
			}
			if _, err := target.AddReviewComment(db.ReviewComment{
				ReviewID: reviewID,
				File:     comment.File,
				Line:     comment.Line,
				Message:  comment.Message,
			}); err != nil {
				return stats, fmt.Errorf("insert comment: %w", err)
			}
			stats.CommentsInserted++
		}
	}

	return stats, nil
}

func loadCandidates(legacy *sql.DB) ([]candidate, int, error) {
	byKey := map[string]*candidate{}
	skippedPending := 0

	rows, err := legacy.Query(`
		SELECT repository, pr_number, COALESCE(pr_title,''), COALESCE(pr_author,''),
			COALESCE(head_sha,''), review_action, COALESCE(review_summary,''), COALESCE(review_comment,''),
			COALESCE(status,'active'), reviewed_at
		FROM pr_reviews`)
	if err != nil {
		return nil, 0, fmt.Errorf("query pr_reviews: %w", err)
	}
	for rows.Next() {
		var c candidate
		var action, status, reviewedAt string
		if err := rows.Scan(&c.Repo, &c.PRNumber, &c.Title, &c.Author, &c.CommitSHA, &action, &c.Summary, &c.GeneralComment, &status, &reviewedAt); err != nil {
			rows.Close()
			return nil, 0, err
		}
		c.Outcome = MapActionToOutcome(action)
		c.Published = true
		c.Closed = status == "merged_or_closed"
		c.CreatedAt = normalizeTS(reviewedAt)
		byKey[candidateKey(c)] = &c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	prows, err := legacy.Query(`
		SELECT repository, pr_number, COALESCE(pr_title,''), COALESCE(pr_author,''),
			COALESCE(head_sha,''), review_action,
			COALESCE(review_summary,''), COALESCE(review_comment,''),
			COALESCE(edited_review_summary,''), COALESCE(edited_review_comment,''),
			COALESCE(inline_comments,''), COALESCE(edited_inline_comments,''),
			COALESCE(status,'pending'), created_at
		FROM pending_approvals`)
	if err != nil {
		return nil, 0, fmt.Errorf("query pending_approvals: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var c candidate
		var action, status, createdAt string
		var summary, comment, editedSummary, editedComment, inlineJSON, editedInlineJSON string
		if err := prows.Scan(&c.Repo, &c.PRNumber, &c.Title, &c.Author, &c.CommitSHA, &action,
			&summary, &comment, &editedSummary, &editedComment, &inlineJSON, &editedInlineJSON, &status, &createdAt); err != nil {
			return nil, 0, err
		}
		if status == "pending" {
			skippedPending++
			continue
		}
		c.Outcome = MapActionToOutcome(action)
		c.Summary = preferNonEmpty(editedSummary, summary)
		c.GeneralComment = preferNonEmpty(editedComment, comment)
		c.Published = status == "approved"
		c.Closed = status == "merged_or_closed"
		c.CreatedAt = normalizeTS(createdAt)
		c.Comments = parseComments(preferNonEmpty(editedInlineJSON, inlineJSON))

		key := candidateKey(c)
		if existing, ok := byKey[key]; ok {
			mergeCandidate(existing, c)
		} else {
			cp := c
			byKey[key] = &cp
		}
	}
	if err := prows.Err(); err != nil {
		return nil, 0, err
	}

	out := make([]candidate, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, *c)
	}
	return out, skippedPending, nil
}

func mergeCandidate(dst *candidate, src candidate) {
	if src.Summary != "" && (dst.Summary == "" || len(src.Summary) > len(dst.Summary)) {
		dst.Summary = src.Summary
	}
	if src.GeneralComment != "" && (dst.GeneralComment == "" || len(src.GeneralComment) > len(dst.GeneralComment)) {
		dst.GeneralComment = src.GeneralComment
	}
	if len(src.Comments) > len(dst.Comments) {
		dst.Comments = src.Comments
	}
	if src.Published {
		dst.Published = true
	}
	if src.Title != "" && dst.Title == "" {
		dst.Title = src.Title
	}
	if src.Author != "" && dst.Author == "" {
		dst.Author = src.Author
	}
	// Prefer earlier timestamp for historical ordering.
	if src.CreatedAt != "" && (dst.CreatedAt == "" || src.CreatedAt < dst.CreatedAt) {
		dst.CreatedAt = src.CreatedAt
	}
}

func candidateKey(c candidate) string {
	return fmt.Sprintf("%s#%d@%s", c.Repo, c.PRNumber, effectiveSHA(c))
}

// effectiveSHA returns a stable dedup token. Empty legacy SHAs get a synthetic
// marker so re-runs remain idempotent.
func effectiveSHA(c candidate) string {
	if c.CommitSHA != "" {
		return c.CommitSHA
	}
	return "legacy:nosha:" + c.CreatedAt + ":" + c.Outcome
}

func preferNonEmpty(preferred, fallback string) string {
	if strings.TrimSpace(preferred) != "" && preferred != "[]" {
		return preferred
	}
	return fallback
}

func parseComments(raw string) []inlineComment {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" || raw == "null" {
		return nil
	}
	var comments []inlineComment
	if err := json.Unmarshal([]byte(raw), &comments); err != nil {
		return nil
	}
	return comments
}

func normalizeTS(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Now().UTC().Format("2006-01-02 15:04:05")
	}
	// Handle RFC3339 and "YYYY-MM-DD HH:MM:SS[.fff]"
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format("2006-01-02 15:04:05")
		}
	}
	if len(raw) >= 19 {
		candidate := strings.Replace(raw[:19], "T", " ", 1)
		if t, err := time.Parse("2006-01-02 15:04:05", candidate); err == nil {
			return t.UTC().Format("2006-01-02 15:04:05")
		}
	}
	return time.Now().UTC().Format("2006-01-02 15:04:05")
}

func ensurePR(target *db.DB, c candidate) (prID int64, created bool, err error) {
	existing, err := target.GetPRByRepoAndNumber(c.Repo, c.PRNumber)
	if err != nil {
		return 0, false, err
	}
	if existing != nil {
		return existing.ID, false, nil
	}

	state := "open"
	if c.Closed {
		state = "closed"
	}
	// Insert directly so we can set historical created_at and needs_review=0
	// without going through UpsertPR (which defaults needs_review and now()).
	res, err := target.Exec(`
		INSERT INTO pull_requests (repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, 0, 0, ?, ?)`,
		c.Repo, c.PRNumber, c.Title, c.Author, c.CommitSHA, state, c.CreatedAt, c.CreatedAt)
	if err != nil {
		return 0, false, err
	}
	id, err := res.LastInsertId()
	return id, true, err
}

func hasReviewForSHA(target *db.DB, prID int64, commitSHA string) (bool, error) {
	var count int
	err := target.QueryRow(`
		SELECT COUNT(*) FROM reviews
		WHERE pull_request_id = ? AND commit_sha = ? AND deleted_at IS NULL`,
		prID, commitSHA).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check existing review: %w", err)
	}
	return count > 0, nil
}

func insertHistoricalReview(target *db.DB, prID int64, c candidate) (int64, error) {
	tx, err := target.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		INSERT INTO review_requests (pull_request_id, status, created_at, updated_at)
		VALUES (?, 'done', ?, ?)`, prID, c.CreatedAt, c.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("review_request: %w", err)
	}
	reqID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	published := 0
	if c.Published {
		published = 1
	}
	res, err = tx.Exec(`
		INSERT INTO reviews (pull_request_id, review_request_id, outcome, commit_sha, summary, general_comment, published, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		prID, reqID, c.Outcome, c.CommitSHA, c.Summary, c.GeneralComment, published, c.CreatedAt, c.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("review: %w", err)
	}
	reviewID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return reviewID, nil
}
