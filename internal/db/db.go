package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{db}, nil
}

func migrate(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS pull_requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo TEXT NOT NULL,
		pr_number INTEGER NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		author TEXT NOT NULL DEFAULT '',
		commit_sha TEXT NOT NULL DEFAULT '',
		draft INTEGER NOT NULL DEFAULT 0,
		state TEXT NOT NULL DEFAULT 'open',
		needs_review INTEGER NOT NULL DEFAULT 1,
		is_outdated INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		deleted_at TEXT
	);

	CREATE UNIQUE INDEX IF NOT EXISTS idx_pr_repo_number ON pull_requests(repo, pr_number);

	CREATE TABLE IF NOT EXISTS review_requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		pull_request_id INTEGER NOT NULL REFERENCES pull_requests(id),
		status TEXT NOT NULL DEFAULT 'pending',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		deleted_at TEXT
	);

	CREATE TABLE IF NOT EXISTS reviews (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		pull_request_id INTEGER NOT NULL REFERENCES pull_requests(id),
		review_request_id INTEGER NOT NULL REFERENCES review_requests(id),
		outcome TEXT NOT NULL,
		summary TEXT NOT NULL DEFAULT '',
		general_comment TEXT NOT NULL DEFAULT '',
		published INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		deleted_at TEXT
	);

	CREATE TABLE IF NOT EXISTS review_comments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		review_id INTEGER NOT NULL REFERENCES reviews(id),
		file TEXT NOT NULL DEFAULT '',
		line INTEGER NOT NULL DEFAULT 0,
		message TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		deleted_at TEXT
	);
	`
	_, err := db.Exec(schema)
	return err
}

func scanPR(row *sql.Row) (PullRequest, error) {
	var pr PullRequest
	var draft int
	var needsReview int
	var outdated int
	var deletedAt sql.NullString
	err := row.Scan(
		&pr.ID, &pr.Repo, &pr.PRNumber, &pr.Title, &pr.Author,
		&pr.CommitSHA, &draft, &pr.State, &needsReview, &outdated,
		&pr.CreatedAt, &pr.UpdatedAt, &deletedAt,
	)
	pr.Draft = draft == 1
	pr.NeedsReview = needsReview == 1
	pr.IsOutdated = outdated == 1
	if deletedAt.Valid {
		t, _ := time.Parse("2006-01-02 15:04:05", deletedAt.String)
		pr.DeletedAt = &t
	}
	return pr, err
}

func scanPRs(rows *sql.Rows) ([]PullRequest, error) {
	var prs []PullRequest
	for rows.Next() {
		var pr PullRequest
		var draft int
		var needsReview int
		var outdated int
		var deletedAt sql.NullString
		err := rows.Scan(
			&pr.ID, &pr.Repo, &pr.PRNumber, &pr.Title, &pr.Author,
			&pr.CommitSHA, &draft, &pr.State, &needsReview, &outdated,
			&pr.CreatedAt, &pr.UpdatedAt, &deletedAt,
		)
		if err != nil {
			return nil, err
		}
		pr.Draft = draft == 1
		pr.NeedsReview = needsReview == 1
		pr.IsOutdated = outdated == 1
		if deletedAt.Valid {
			t, _ := time.Parse("2006-01-02 15:04:05", deletedAt.String)
			pr.DeletedAt = &t
		}
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}

func (d *DB) UpsertPR(pr PullRequest) (int64, error) {
	var existingID int64
	err := d.QueryRow("SELECT id FROM pull_requests WHERE repo = ? AND pr_number = ? AND deleted_at IS NULL", pr.Repo, pr.PRNumber).Scan(&existingID)
	if err == sql.ErrNoRows {
		res, err := d.Exec(`INSERT INTO pull_requests (repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			pr.Repo, pr.PRNumber, pr.Title, pr.Author, pr.CommitSHA, boolToInt(pr.Draft), pr.State, boolToInt(pr.NeedsReview), boolToInt(pr.IsOutdated))
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	_, err = d.Exec(`UPDATE pull_requests SET title=?, author=?, commit_sha=?, draft=?, state=?, needs_review=?, is_outdated=?, updated_at=datetime('now') WHERE id=?`,
		pr.Title, pr.Author, pr.CommitSHA, boolToInt(pr.Draft), pr.State, boolToInt(pr.NeedsReview), boolToInt(pr.IsOutdated), existingID)
	return existingID, err
}

func (d *DB) GetPRByRepoAndNumber(repo string, number int) (*PullRequest, error) {
	row := d.QueryRow("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at FROM pull_requests WHERE repo = ? AND pr_number = ? AND deleted_at IS NULL", repo, number)
	pr, err := scanPR(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pr, nil
}

func (d *DB) GetPR(id int64) (*PullRequest, error) {
	row := d.QueryRow("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at FROM pull_requests WHERE id = ? AND deleted_at IS NULL", id)
	pr, err := scanPR(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pr, nil
}

func (d *DB) ListOpenPRs() ([]PullRequest, error) {
	rows, err := d.Query("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at FROM pull_requests WHERE state = 'open' AND deleted_at IS NULL ORDER BY updated_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPRs(rows)
}

func (d *DB) ListPRsNeedingReview() ([]PullRequest, error) {
	rows, err := d.Query("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at FROM pull_requests WHERE state = 'open' AND needs_review = 1 AND is_outdated = 0 AND deleted_at IS NULL ORDER BY updated_at ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPRs(rows)
}

func (d *DB) SetPRNeedsReview(id int64, needs bool) error {
	_, err := d.Exec("UPDATE pull_requests SET needs_review = ?, updated_at = datetime('now') WHERE id = ?", boolToInt(needs), id)
	return err
}

func (d *DB) MarkPROutdated(id int64) error {
	_, err := d.Exec("UPDATE pull_requests SET is_outdated = 1, updated_at = datetime('now') WHERE id = ?", id)
	return err
}

func (d *DB) ClosePR(id int64) error {
	_, err := d.Exec("UPDATE pull_requests SET state = 'closed', needs_review = 0, updated_at = datetime('now') WHERE id = ?", id)
	return err
}

func (d *DB) SoftDeletePR(id int64) error {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, err := d.Exec("UPDATE pull_requests SET deleted_at = ?, updated_at = ? WHERE id = ?", now, now, id)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (d *DB) CreateReviewRequest(prID int64) (int64, error) {
	res, err := d.Exec("INSERT INTO review_requests (pull_request_id, status) VALUES (?, 'pending')", prID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) GetNextPendingReviewRequest() (*ReviewRequest, error) {
	row := d.QueryRow("SELECT id, pull_request_id, status, created_at, updated_at, deleted_at FROM review_requests WHERE status = 'pending' AND deleted_at IS NULL ORDER BY created_at ASC LIMIT 1")
	var rr ReviewRequest
	var deletedAt sql.NullString
	err := row.Scan(&rr.ID, &rr.PullRequestID, &rr.Status, &rr.CreatedAt, &rr.UpdatedAt, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		t, _ := time.Parse("2006-01-02 15:04:05", deletedAt.String)
		rr.DeletedAt = &t
	}
	return &rr, nil
}

func (d *DB) UpdateReviewRequestStatus(id int64, status string) error {
	_, err := d.Exec("UPDATE review_requests SET status = ?, updated_at = datetime('now') WHERE id = ?", status, id)
	return err
}

func (d *DB) ListReviewRequests() ([]ReviewRequest, error) {
	rows, err := d.Query("SELECT id, pull_request_id, status, created_at, updated_at, deleted_at FROM review_requests WHERE deleted_at IS NULL ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rrs []ReviewRequest
	for rows.Next() {
		var rr ReviewRequest
		var deletedAt sql.NullString
		err := rows.Scan(&rr.ID, &rr.PullRequestID, &rr.Status, &rr.CreatedAt, &rr.UpdatedAt, &deletedAt)
		if err != nil {
			return nil, err
		}
		if deletedAt.Valid {
			t, _ := time.Parse("2006-01-02 15:04:05", deletedAt.String)
			rr.DeletedAt = &t
		}
		rrs = append(rrs, rr)
	}
	return rrs, rows.Err()
}

func (d *DB) CreateReview(r Review) (int64, error) {
	res, err := d.Exec(`INSERT INTO reviews (pull_request_id, review_request_id, outcome, summary, general_comment) VALUES (?, ?, ?, ?, ?)`,
		r.PullRequestID, r.ReviewRequestID, r.Outcome, r.Summary, r.GeneralComment)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) GetReview(id int64) (*Review, error) {
	row := d.QueryRow("SELECT id, pull_request_id, review_request_id, outcome, summary, general_comment, published, created_at, updated_at, deleted_at FROM reviews WHERE id = ? AND deleted_at IS NULL", id)
	var r Review
	var deletedAt sql.NullString
	err := row.Scan(&r.ID, &r.PullRequestID, &r.ReviewRequestID, &r.Outcome, &r.Summary, &r.GeneralComment, &r.Published, &r.CreatedAt, &r.UpdatedAt, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		t, _ := time.Parse("2006-01-02 15:04:05", deletedAt.String)
		r.DeletedAt = &t
	}
	return &r, nil
}

func (d *DB) GetReviewByRequestID(requestID int64) (*Review, error) {
	row := d.QueryRow("SELECT id, pull_request_id, review_request_id, outcome, summary, general_comment, published, created_at, updated_at, deleted_at FROM reviews WHERE review_request_id = ? AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1", requestID)
	var r Review
	var deletedAt sql.NullString
	err := row.Scan(&r.ID, &r.PullRequestID, &r.ReviewRequestID, &r.Outcome, &r.Summary, &r.GeneralComment, &r.Published, &r.CreatedAt, &r.UpdatedAt, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if deletedAt.Valid {
		t, _ := time.Parse("2006-01-02 15:04:05", deletedAt.String)
		r.DeletedAt = &t
	}
	return &r, nil
}

func (d *DB) ListReviewsForPR(prID int64) ([]Review, error) {
	rows, err := d.Query("SELECT id, pull_request_id, review_request_id, outcome, summary, general_comment, published, created_at, updated_at, deleted_at FROM reviews WHERE pull_request_id = ? AND deleted_at IS NULL ORDER BY created_at DESC", prID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reviews []Review
	for rows.Next() {
		var r Review
		var deletedAt sql.NullString
		err := rows.Scan(&r.ID, &r.PullRequestID, &r.ReviewRequestID, &r.Outcome, &r.Summary, &r.GeneralComment, &r.Published, &r.CreatedAt, &r.UpdatedAt, &deletedAt)
		if err != nil {
			return nil, err
		}
		if deletedAt.Valid {
			t, _ := time.Parse("2006-01-02 15:04:05", deletedAt.String)
			r.DeletedAt = &t
		}
		reviews = append(reviews, r)
	}
	return reviews, rows.Err()
}

func (d *DB) PublishReview(id int64) error {
	_, err := d.Exec("UPDATE reviews SET published = 1, updated_at = datetime('now') WHERE id = ?", id)
	return err
}

func (d *DB) AddReviewComment(c ReviewComment) (int64, error) {
	res, err := d.Exec("INSERT INTO review_comments (review_id, file, line, message) VALUES (?, ?, ?, ?)",
		c.ReviewID, c.File, c.Line, c.Message)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) ListReviewComments(reviewID int64) ([]ReviewComment, error) {
	rows, err := d.Query("SELECT id, review_id, file, line, message, created_at, updated_at, deleted_at FROM review_comments WHERE review_id = ? AND deleted_at IS NULL ORDER BY created_at ASC", reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var comments []ReviewComment
	for rows.Next() {
		var c ReviewComment
		var deletedAt sql.NullString
		err := rows.Scan(&c.ID, &c.ReviewID, &c.File, &c.Line, &c.Message, &c.CreatedAt, &c.UpdatedAt, &deletedAt)
		if err != nil {
			return nil, err
		}
		if deletedAt.Valid {
			t, _ := time.Parse("2006-01-02 15:04:05", deletedAt.String)
			c.DeletedAt = &t
		}
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

func (d *DB) DeleteReviewComment(id int64) error {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	_, err := d.Exec("UPDATE review_comments SET deleted_at = ?, updated_at = ? WHERE id = ?", now, now, id)
	return err
}

func (d *DB) UpdateReviewComment(id int64, message string) error {
	_, err := d.Exec("UPDATE review_comments SET message = ?, updated_at = datetime('now') WHERE id = ?", message, id)
	return err
}

func (d *DB) CountReviewsByOutcomeSince(outcome string, since time.Time) (int, error) {
	var count int
	err := d.QueryRow("SELECT COUNT(*) FROM reviews WHERE outcome = ? AND created_at >= ? AND deleted_at IS NULL", outcome, since.Format("2006-01-02 15:04:05")).Scan(&count)
	return count, err
}

func (d *DB) CountReviewsByAuthorSince(author string, since time.Time) (int, error) {
	var count int
	err := d.QueryRow("SELECT COUNT(*) FROM reviews r JOIN pull_requests p ON r.pull_request_id = p.id WHERE p.author = ? AND r.created_at >= ? AND r.deleted_at IS NULL", author, since.Format("2006-01-02 15:04:05")).Scan(&count)
	return count, err
}

func (d *DB) CountReviewsByRepoSince(repo string, since time.Time) (int, error) {
	var count int
	err := d.QueryRow("SELECT COUNT(*) FROM reviews r JOIN pull_requests p ON r.pull_request_id = p.id WHERE p.repo = ? AND r.created_at >= ? AND r.deleted_at IS NULL", repo, since.Format("2006-01-02 15:04:05")).Scan(&count)
	return count, err
}
