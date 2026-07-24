package migrate

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/sephriot/code-reviewer/internal/db"
	_ "modernc.org/sqlite"
)

func TestMapActionToOutcome(t *testing.T) {
	cases := map[string]string{
		"approve_without_comment": db.ReviewOutcomeApproveWithoutComments,
		"approve_with_comment":    db.ReviewOutcomeApproveWithComments,
		"request_changes":         db.ReviewOutcomeChangesRequested,
		"requires_human_review":   db.ReviewOutcomeHumanReview,
		"weird":                   db.ReviewOutcomeToolFailed,
	}
	for in, want := range cases {
		if got := MapActionToOutcome(in); got != want {
			t.Errorf("MapActionToOutcome(%q)=%q want %q", in, got, want)
		}
	}
}

func TestMigrateImportsPrReviewsAndPendingComments(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	targetPath := filepath.Join(t.TempDir(), "target.db")
	seedLegacy(t, legacyPath)

	target, err := db.Open(targetPath)
	if err != nil {
		t.Fatalf("open target: %v", err)
	}
	defer target.Close()

	liveID, err := target.UpsertPR(db.PullRequest{
		Repo: "org/repo", PRNumber: 1, Title: "LIVE TITLE", Author: "live",
		CommitSHA: "aaa111", State: "open", NeedsReview: true,
	})
	if err != nil {
		t.Fatalf("seed live pr: %v", err)
	}

	stats, err := Run(legacyPath, targetPath, false)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.ReviewsInserted < 2 {
		t.Fatalf("expected >=2 reviews inserted, got %+v", stats)
	}

	pr, err := target.GetPR(liveID)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if pr.Title != "LIVE TITLE" || !pr.NeedsReview {
		t.Fatalf("live PR was clobbered: %+v", pr)
	}

	reviews, err := target.ListReviewsForPR(liveID)
	if err != nil {
		t.Fatalf("ListReviewsForPR: %v", err)
	}
	var found bool
	for _, r := range reviews {
		if r.CommitSHA == "deadbeef" && r.Outcome == db.ReviewOutcomeApproveWithComments && r.Published {
			found = true
			comments, err := target.ListReviewComments(r.ID)
			if err != nil {
				t.Fatalf("comments: %v", err)
			}
			if len(comments) != 1 || comments[0].File != "a.go" || comments[0].Line != 10 {
				t.Fatalf("unexpected comments: %+v", comments)
			}
			if r.Summary != "edited summary" {
				t.Fatalf("expected edited summary, got %q", r.Summary)
			}
		}
	}
	if !found {
		t.Fatalf("missing migrated review for deadbeef; reviews=%+v", reviews)
	}

	pr2, err := target.GetPRByRepoAndNumber("org/other", 2)
	if err != nil {
		t.Fatalf("GetPRByRepoAndNumber other: %v", err)
	}
	if pr2 == nil {
		t.Fatal("expected other PR to exist")
	}
	if pr2.NeedsReview {
		t.Fatal("migrated PR should have needs_review=0")
	}
	reviews2, err := target.ListReviewsForPR(pr2.ID)
	if err != nil {
		t.Fatalf("reviews2: %v", err)
	}
	if len(reviews2) != 1 || reviews2[0].Outcome != db.ReviewOutcomeChangesRequested || reviews2[0].Published {
		t.Fatalf("unexpected other reviews: %+v", reviews2)
	}

	skipped, err := target.GetPRByRepoAndNumber("org/skip", 3)
	if err != nil {
		t.Fatalf("skip lookup: %v", err)
	}
	if skipped != nil {
		t.Fatal("pending status should be skipped")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	targetPath := filepath.Join(t.TempDir(), "target.db")
	seedLegacy(t, legacyPath)
	if _, err := db.Open(targetPath); err != nil {
		t.Fatalf("open target: %v", err)
	}

	first, err := Run(legacyPath, targetPath, false)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := Run(legacyPath, targetPath, false)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.ReviewsInserted != 0 || second.CommentsInserted != 0 {
		t.Fatalf("second run should insert nothing, got %+v (first=%+v)", second, first)
	}
}

func seedLegacy(t *testing.T, path string) {
	t.Helper()
	dbConn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	defer dbConn.Close()

	stmts := []string{
		`CREATE TABLE pr_reviews (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repository TEXT NOT NULL,
			pr_number INTEGER NOT NULL,
			pr_title TEXT,
			pr_author TEXT,
			review_action TEXT NOT NULL,
			review_reason TEXT,
			review_comment TEXT,
			review_summary TEXT,
			inline_comments_count INTEGER DEFAULT 0,
			reviewed_at TIMESTAMP NOT NULL,
			pr_updated_at TIMESTAMP,
			head_sha TEXT,
			base_sha TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			status TEXT NOT NULL DEFAULT 'active'
		)`,
		`CREATE TABLE pending_approvals (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repository TEXT NOT NULL,
			pr_number INTEGER NOT NULL,
			pr_title TEXT,
			pr_author TEXT,
			pr_url TEXT NOT NULL,
			review_action TEXT NOT NULL,
			review_comment TEXT,
			review_summary TEXT,
			review_reason TEXT,
			inline_comments TEXT,
			inline_comments_count INTEGER DEFAULT 0,
			edited_review_comment TEXT,
			edited_review_summary TEXT,
			edited_inline_comments TEXT,
			head_sha TEXT NOT NULL,
			base_sha TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			status TEXT DEFAULT 'pending'
		)`,
		`INSERT INTO pr_reviews (repository, pr_number, pr_title, pr_author, review_action, review_summary, review_comment, head_sha, reviewed_at, status)
		 VALUES ('org/repo', 1, 'Old title', 'alice', 'approve_with_comment', 'old summary', 'old comment', 'deadbeef', '2026-01-01 12:00:00', 'active')`,
		`INSERT INTO pending_approvals (repository, pr_number, pr_title, pr_author, pr_url, review_action, review_summary, review_comment,
			inline_comments, edited_review_summary, edited_inline_comments, head_sha, status, created_at)
		 VALUES ('org/repo', 1, 'Old title', 'alice', 'https://example/1', 'approve_with_comment', 'old summary', 'old comment',
			'[{"file":"a.go","line":10,"message":"fix me"}]', 'edited summary', '[]', 'deadbeef', 'approved', '2026-01-01 12:05:00')`,
		`INSERT INTO pending_approvals (repository, pr_number, pr_title, pr_author, pr_url, review_action, review_summary,
			inline_comments, head_sha, status, created_at)
		 VALUES ('org/other', 2, 'Other', 'bob', 'https://example/2', 'request_changes', 'needs work',
			'[{"file":"b.go","line":2,"message":"nope"}]', 'cafebabe', 'expired', '2026-02-01 00:00:00')`,
		`INSERT INTO pending_approvals (repository, pr_number, pr_title, pr_author, pr_url, review_action, head_sha, status, created_at)
		 VALUES ('org/skip', 3, 'Skip', 'x', 'https://example/3', 'approve_with_comment', 'skipsha', 'pending', '2026-03-01 00:00:00')`,
	}
	for _, s := range stmts {
		if _, err := dbConn.Exec(s); err != nil {
			t.Fatalf("seed exec: %v\nstmt: %s", err, s[:min(80, len(s))])
		}
	}
}

func TestMigrateEmptySHAIdempotent(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	targetPath := filepath.Join(t.TempDir(), "target.db")
	dbConn, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dbConn.Close()
	for _, s := range []string{
		`CREATE TABLE pr_reviews (
			id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, pr_title TEXT, pr_author TEXT,
			review_action TEXT, review_reason TEXT, review_comment TEXT, review_summary TEXT,
			inline_comments_count INTEGER DEFAULT 0, reviewed_at TIMESTAMP, pr_updated_at TIMESTAMP,
			head_sha TEXT, base_sha TEXT, created_at TIMESTAMP, status TEXT)`,
		`CREATE TABLE pending_approvals (
			id INTEGER PRIMARY KEY, repository TEXT, pr_number INTEGER, pr_title TEXT, pr_author TEXT,
			pr_url TEXT, review_action TEXT, review_comment TEXT, review_summary TEXT, review_reason TEXT,
			inline_comments TEXT, inline_comments_count INTEGER, edited_review_comment TEXT,
			edited_review_summary TEXT, edited_inline_comments TEXT, head_sha TEXT, base_sha TEXT,
			created_at TIMESTAMP, status TEXT)`,
		`INSERT INTO pr_reviews (repository, pr_number, pr_title, pr_author, review_action, review_summary, review_comment, head_sha, reviewed_at, status)
		 VALUES ('org/nosha', 9, 'No SHA', 'z', 'approve_without_comment', '', '', '', '2026-04-01 00:00:00', 'active')`,
	} {
		if _, err := dbConn.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Open(targetPath); err != nil {
		t.Fatal(err)
	}
	first, err := Run(legacyPath, targetPath, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(legacyPath, targetPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.ReviewsInserted != 1 || second.ReviewsInserted != 0 {
		t.Fatalf("empty-sha idempotency failed: first=%+v second=%+v", first, second)
	}
}
