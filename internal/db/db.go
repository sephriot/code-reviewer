package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type scanTime time.Time

func (t *scanTime) Scan(value interface{}) error {
	if value == nil {
		*t = scanTime(time.Time{})
		return nil
	}
	var s string
	switch v := value.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("cannot scan %T into scanTime", value)
	}
	parsed, err := time.Parse("2006-01-02 15:04:05", s)
	if err != nil {
		return err
	}
	*t = scanTime(parsed)
	return nil
}

type nullScanTime struct {
	Time  time.Time
	Valid bool
}

func (t *nullScanTime) Scan(value interface{}) error {
	if value == nil {
		t.Valid = false
		return nil
	}
	t.Valid = true
	return (*scanTime)(&t.Time).Scan(value)
}

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
		published INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at TEXT NOT NULL DEFAULT (datetime('now')),
		deleted_at TEXT
	);
	`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}
	// add columns if missing (idempotent)
	db.Exec("ALTER TABLE reviews ADD COLUMN commit_sha TEXT NOT NULL DEFAULT ''")
	db.Exec("ALTER TABLE pull_requests ADD COLUMN filtered_reason TEXT")
	db.Exec("ALTER TABLE review_comments ADD COLUMN published INTEGER NOT NULL DEFAULT 0")
	db.Exec("ALTER TABLE pull_requests ADD COLUMN gh_updated_at TEXT")
	// Closed/merged was briefly mis-tagged as filtered_reason='state'; clear it.
	if res, err := db.Exec("UPDATE pull_requests SET filtered_reason = NULL WHERE filtered_reason = 'state'"); err != nil {
		return err
	} else if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("db: cleared filtered_reason=state on %d PRs (moved to history)", n)
	}
	res, err := db.Exec("UPDATE review_requests SET status = 'pending', updated_at = datetime('now') WHERE status = 'in_progress'")
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("db: reset %d orphaned in_progress review_requests to pending", n)
	}

	return nil
}

func scanPR(row *sql.Row) (PullRequest, error) {
	var pr PullRequest
	var draft int
	var needsReview int
	var outdated int
	var createdAt scanTime
	var updatedAt scanTime
	var deletedAt nullScanTime
	var filteredReason *string
	var ghUpdatedAt nullScanTime
	err := row.Scan(
		&pr.ID, &pr.Repo, &pr.PRNumber, &pr.Title, &pr.Author,
		&pr.CommitSHA, &draft, &pr.State, &needsReview, &outdated,
		&createdAt, &updatedAt, &deletedAt, &filteredReason, &ghUpdatedAt,
	)
	pr.Draft = draft == 1
	pr.NeedsReview = needsReview == 1
	pr.IsOutdated = outdated == 1
	pr.CreatedAt = time.Time(createdAt)
	pr.UpdatedAt = time.Time(updatedAt)
	if deletedAt.Valid {
		pr.DeletedAt = &deletedAt.Time
	}
	if filteredReason != nil {
		pr.FilteredReason = *filteredReason
	}
	if ghUpdatedAt.Valid {
		pr.GhUpdatedAt = ghUpdatedAt.Time
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
		var createdAt scanTime
		var updatedAt scanTime
		var deletedAt nullScanTime
		var filteredReason *string
		var ghUpdatedAt nullScanTime
		err := rows.Scan(
			&pr.ID, &pr.Repo, &pr.PRNumber, &pr.Title, &pr.Author,
			&pr.CommitSHA, &draft, &pr.State, &needsReview, &outdated,
			&createdAt, &updatedAt, &deletedAt, &filteredReason, &ghUpdatedAt,
		)
		if err != nil {
			return nil, err
		}
		pr.Draft = draft == 1
		pr.NeedsReview = needsReview == 1
		pr.IsOutdated = outdated == 1
		pr.CreatedAt = time.Time(createdAt)
		pr.UpdatedAt = time.Time(updatedAt)
		if deletedAt.Valid {
			pr.DeletedAt = &deletedAt.Time
		}
		if filteredReason != nil {
			pr.FilteredReason = *filteredReason
		}
		if ghUpdatedAt.Valid {
			pr.GhUpdatedAt = ghUpdatedAt.Time
		}
		prs = append(prs, pr)
	}
	return prs, rows.Err()
}

func (d *DB) UpsertPR(pr PullRequest) (int64, error) {
	var existingID int64
	err := d.QueryRow("SELECT id FROM pull_requests WHERE repo = ? AND pr_number = ? AND deleted_at IS NULL", pr.Repo, pr.PRNumber).Scan(&existingID)
	if err == sql.ErrNoRows {
		res, err := d.Exec(`INSERT INTO pull_requests (repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, filtered_reason, gh_updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			pr.Repo, pr.PRNumber, pr.Title, pr.Author, pr.CommitSHA, boolToInt(pr.Draft), pr.State, boolToInt(pr.NeedsReview), boolToInt(pr.IsOutdated), nullableStr(pr.FilteredReason), nullableTime(pr.GhUpdatedAt))
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	if !pr.GhUpdatedAt.IsZero() {
		_, err = d.Exec(`UPDATE pull_requests SET title=?, author=?, commit_sha=?, draft=?, state=?, needs_review=?, is_outdated=?, filtered_reason=?, gh_updated_at=?, updated_at=datetime('now') WHERE id=?`,
			pr.Title, pr.Author, pr.CommitSHA, boolToInt(pr.Draft), pr.State, boolToInt(pr.NeedsReview), boolToInt(pr.IsOutdated), nullableStr(pr.FilteredReason), nullableTime(pr.GhUpdatedAt), existingID)
	} else {
		_, err = d.Exec(`UPDATE pull_requests SET title=?, author=?, commit_sha=?, draft=?, state=?, needs_review=?, is_outdated=?, filtered_reason=?, updated_at=datetime('now') WHERE id=?`,
			pr.Title, pr.Author, pr.CommitSHA, boolToInt(pr.Draft), pr.State, boolToInt(pr.NeedsReview), boolToInt(pr.IsOutdated), nullableStr(pr.FilteredReason), existingID)
	}
	return existingID, err
}

func (d *DB) GetPRByRepoAndNumber(repo string, number int) (*PullRequest, error) {
	row := d.QueryRow("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at, filtered_reason, gh_updated_at FROM pull_requests WHERE repo = ? AND pr_number = ? AND deleted_at IS NULL", repo, number)
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
	row := d.QueryRow("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at, filtered_reason, gh_updated_at FROM pull_requests WHERE id = ? AND deleted_at IS NULL", id)
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
	rows, err := d.Query("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at, filtered_reason, gh_updated_at FROM pull_requests WHERE state = 'open' AND deleted_at IS NULL ORDER BY COALESCE(gh_updated_at, updated_at) DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPRs(rows)
}

func (d *DB) ListPRsNeedingReview() ([]PullRequest, error) {
	rows, err := d.Query("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at, filtered_reason, gh_updated_at FROM pull_requests WHERE state = 'open' AND needs_review = 1 AND is_outdated = 0 AND filtered_reason IS NULL AND deleted_at IS NULL ORDER BY COALESCE(gh_updated_at, updated_at) DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPRs(rows)
}

func (d *DB) ListFilteredPRs() ([]PullRequest, error) {
	rows, err := d.Query("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at, filtered_reason, gh_updated_at FROM pull_requests WHERE state = 'open' AND filtered_reason IS NOT NULL AND deleted_at IS NULL ORDER BY COALESCE(gh_updated_at, updated_at) DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPRs(rows)
}

func (d *DB) ListHistoryPRs() ([]PullRequest, error) {
	rows, err := d.Query("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at, filtered_reason, gh_updated_at FROM pull_requests WHERE deleted_at IS NULL AND (state != 'open' OR (state = 'open' AND needs_review = 0 AND filtered_reason IS NULL)) ORDER BY COALESCE(gh_updated_at, updated_at) DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPRs(rows)
}

func (d *DB) ListOpenActivePRs() ([]PullRequest, error) {
	rows, err := d.Query("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at, filtered_reason, gh_updated_at FROM pull_requests WHERE state = 'open' AND filtered_reason IS NULL AND deleted_at IS NULL ORDER BY COALESCE(gh_updated_at, updated_at) DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPRs(rows)
}

func (d *DB) ListPRsByState(state string) ([]PullRequest, error) {
	rows, err := d.Query("SELECT id, repo, pr_number, title, author, commit_sha, draft, state, needs_review, is_outdated, created_at, updated_at, deleted_at, filtered_reason, gh_updated_at FROM pull_requests WHERE state = ? AND deleted_at IS NULL ORDER BY COALESCE(gh_updated_at, updated_at) DESC", state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPRs(rows)
}

func (d *DB) ListClosedPRs() ([]PullRequest, error) {
	return d.ListPRsByState(PRStateClosed)
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
	_, err := d.Exec("UPDATE pull_requests SET state = 'closed', needs_review = 0, filtered_reason = NULL, updated_at = datetime('now') WHERE id = ?", id)
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

func nullableStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

func (d *DB) CreateReviewRequest(prID int64) (int64, error) {
	res, err := d.Exec("INSERT INTO review_requests (pull_request_id, status) VALUES (?, 'pending')", prID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) GetPendingRequestByPR(prID int64) (*ReviewRequest, error) {
	row := d.QueryRow("SELECT id, pull_request_id, status, created_at, updated_at, deleted_at FROM review_requests WHERE pull_request_id = ? AND deleted_at IS NULL AND status != 'done' ORDER BY created_at ASC LIMIT 1", prID)
	var rr ReviewRequest
	var createdAt scanTime
	var updatedAt scanTime
	var deletedAt nullScanTime
	err := row.Scan(&rr.ID, &rr.PullRequestID, &rr.Status, &createdAt, &updatedAt, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rr.CreatedAt = time.Time(createdAt)
	rr.UpdatedAt = time.Time(updatedAt)
	if deletedAt.Valid {
		rr.DeletedAt = &deletedAt.Time
	}
	return &rr, nil
}

func (d *DB) GetNextPendingReviewRequest() (*ReviewRequest, error) {
	row := d.QueryRow("SELECT id, pull_request_id, status, created_at, updated_at, deleted_at FROM review_requests WHERE status = 'pending' AND deleted_at IS NULL AND NOT EXISTS (SELECT 1 FROM review_requests WHERE status = 'in_progress' AND deleted_at IS NULL) ORDER BY created_at ASC LIMIT 1")
	var rr ReviewRequest
	var createdAt scanTime
	var updatedAt scanTime
	var deletedAt nullScanTime
	err := row.Scan(&rr.ID, &rr.PullRequestID, &rr.Status, &createdAt, &updatedAt, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rr.CreatedAt = time.Time(createdAt)
	rr.UpdatedAt = time.Time(updatedAt)
	if deletedAt.Valid {
		rr.DeletedAt = &deletedAt.Time
	}
	return &rr, nil
}

func (d *DB) ResetStaleReviewRequests(timeout time.Duration) (int64, error) {
	buffer := 5 * time.Minute
	threshold := timeout + buffer
	modifier := fmt.Sprintf("-%d seconds", int64(threshold.Seconds()))
	res, err := d.Exec("UPDATE review_requests SET status = 'pending', updated_at = datetime('now') WHERE status = 'in_progress' AND deleted_at IS NULL AND updated_at < datetime('now', ?)", modifier)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *DB) UpdateReviewRequestStatus(id int64, status string) error {
	_, err := d.Exec("UPDATE review_requests SET status = ?, updated_at = datetime('now') WHERE id = ?", status, id)
	return err
}

func (d *DB) GetReviewRequest(id int64) (*ReviewRequest, error) {
	row := d.QueryRow("SELECT id, pull_request_id, status, created_at, updated_at, deleted_at FROM review_requests WHERE id = ? AND deleted_at IS NULL", id)
	var rr ReviewRequest
	var createdAt scanTime
	var updatedAt scanTime
	var deletedAt nullScanTime
	err := row.Scan(&rr.ID, &rr.PullRequestID, &rr.Status, &createdAt, &updatedAt, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rr.CreatedAt = time.Time(createdAt)
	rr.UpdatedAt = time.Time(updatedAt)
	if deletedAt.Valid {
		rr.DeletedAt = &deletedAt.Time
	}
	return &rr, nil
}

func (d *DB) SoftDeleteReviewRequest(id int64) error {
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	res, err := d.Exec(
		"UPDATE review_requests SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL AND status != 'done'",
		now, now, id,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DB) ListReviewRequests() ([]ReviewRequest, error) {
	rows, err := d.Query("SELECT id, pull_request_id, status, created_at, updated_at, deleted_at FROM review_requests WHERE deleted_at IS NULL AND status != 'done' ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rrs []ReviewRequest
	for rows.Next() {
		var rr ReviewRequest
		var createdAt scanTime
		var updatedAt scanTime
		var deletedAt nullScanTime
		err := rows.Scan(&rr.ID, &rr.PullRequestID, &rr.Status, &createdAt, &updatedAt, &deletedAt)
		if err != nil {
			return nil, err
		}
		rr.CreatedAt = time.Time(createdAt)
		rr.UpdatedAt = time.Time(updatedAt)
		if deletedAt.Valid {
			rr.DeletedAt = &deletedAt.Time
		}
		rrs = append(rrs, rr)
	}
	return rrs, rows.Err()
}

func (d *DB) CreateReview(r Review) (int64, error) {
	res, err := d.Exec(`INSERT INTO reviews (pull_request_id, review_request_id, outcome, commit_sha, summary, general_comment) VALUES (?, ?, ?, ?, ?, ?)`,
		r.PullRequestID, r.ReviewRequestID, r.Outcome, r.CommitSHA, r.Summary, r.GeneralComment)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) scanReview(scanner func(dest ...interface{}) error) (Review, error) {
	var r Review
	var createdAt scanTime
	var updatedAt scanTime
	var deletedAt nullScanTime
	err := scanner(&r.ID, &r.PullRequestID, &r.ReviewRequestID, &r.Outcome, &r.CommitSHA, &r.Summary, &r.GeneralComment, &r.Published, &createdAt, &updatedAt, &deletedAt)
	if err != nil {
		return r, err
	}
	r.CreatedAt = time.Time(createdAt)
	r.UpdatedAt = time.Time(updatedAt)
	if deletedAt.Valid {
		r.DeletedAt = &deletedAt.Time
	}
	return r, nil
}

func (d *DB) GetReview(id int64) (*Review, error) {
	r, err := d.scanReview(d.QueryRow("SELECT id, pull_request_id, review_request_id, outcome, commit_sha, summary, general_comment, published, created_at, updated_at, deleted_at FROM reviews WHERE id = ? AND deleted_at IS NULL", id).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) GetReviewByRequestID(requestID int64) (*Review, error) {
	r, err := d.scanReview(d.QueryRow("SELECT id, pull_request_id, review_request_id, outcome, commit_sha, summary, general_comment, published, created_at, updated_at, deleted_at FROM reviews WHERE review_request_id = ? AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1", requestID).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) GetLatestReviewByPR(prID int64) (*Review, error) {
	r, err := d.scanReview(d.QueryRow("SELECT id, pull_request_id, review_request_id, outcome, commit_sha, summary, general_comment, published, created_at, updated_at, deleted_at FROM reviews WHERE pull_request_id = ? AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1", prID).Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) ListReviewsForPR(prID int64) ([]Review, error) {
	rows, err := d.Query("SELECT id, pull_request_id, review_request_id, outcome, commit_sha, summary, general_comment, published, created_at, updated_at, deleted_at FROM reviews WHERE pull_request_id = ? AND deleted_at IS NULL ORDER BY created_at DESC", prID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reviews []Review
	for rows.Next() {
		r, err := d.scanReview(rows.Scan)
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, r)
	}
	return reviews, rows.Err()
}

func (d *DB) PublishReview(id int64) error {
	_, err := d.Exec("UPDATE reviews SET published = 1, updated_at = datetime('now') WHERE id = ?", id)
	return err
}

func (d *DB) ListPublishedReviews() ([]PublishedReviewView, error) {
	rows, err := d.Query("SELECT r.id, r.pull_request_id, r.review_request_id, r.outcome, r.summary, r.general_comment, r.published, r.created_at, r.updated_at, r.deleted_at, p.repo, p.pr_number, p.title, p.author FROM reviews r JOIN pull_requests p ON r.pull_request_id = p.id WHERE r.published = 1 AND r.deleted_at IS NULL ORDER BY r.created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []PublishedReviewView
	for rows.Next() {
		var v PublishedReviewView
		var createdAt scanTime
		var updatedAt scanTime
		var deletedAt nullScanTime
		err := rows.Scan(&v.ID, &v.PullRequestID, &v.ReviewRequestID, &v.Outcome, &v.Summary, &v.GeneralComment, &v.Published, &createdAt, &updatedAt, &deletedAt, &v.Repo, &v.PRNumber, &v.PRTitle, &v.PRAuthor)
		if err != nil {
			return nil, err
		}
		v.CreatedAt = time.Time(createdAt)
		v.UpdatedAt = time.Time(updatedAt)
		if deletedAt.Valid {
			v.DeletedAt = &deletedAt.Time
		}
		views = append(views, v)
	}
	return views, rows.Err()
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
	rows, err := d.Query("SELECT id, review_id, file, line, message, published, created_at, updated_at, deleted_at FROM review_comments WHERE review_id = ? AND deleted_at IS NULL ORDER BY created_at ASC", reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var comments []ReviewComment
	for rows.Next() {
		var c ReviewComment
		var published int
		var createdAt scanTime
		var updatedAt scanTime
		var deletedAt nullScanTime
		err := rows.Scan(&c.ID, &c.ReviewID, &c.File, &c.Line, &c.Message, &published, &createdAt, &updatedAt, &deletedAt)
		if err != nil {
			return nil, err
		}
		c.Published = published != 0
		c.CreatedAt = time.Time(createdAt)
		c.UpdatedAt = time.Time(updatedAt)
		if deletedAt.Valid {
			c.DeletedAt = &deletedAt.Time
		}
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

func (d *DB) GetReviewComment(id int64) (*ReviewComment, error) {
	var c ReviewComment
	var published int
	var createdAt scanTime
	var updatedAt scanTime
	var deletedAt nullScanTime
	err := d.QueryRow(
		"SELECT id, review_id, file, line, message, published, created_at, updated_at, deleted_at FROM review_comments WHERE id = ? AND deleted_at IS NULL",
		id,
	).Scan(&c.ID, &c.ReviewID, &c.File, &c.Line, &c.Message, &published, &createdAt, &updatedAt, &deletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Published = published != 0
	c.CreatedAt = time.Time(createdAt)
	c.UpdatedAt = time.Time(updatedAt)
	if deletedAt.Valid {
		c.DeletedAt = &deletedAt.Time
	}
	return &c, nil
}

func (d *DB) PublishReviewComment(id int64) error {
	_, err := d.Exec("UPDATE review_comments SET published = 1, updated_at = datetime('now') WHERE id = ? AND deleted_at IS NULL", id)
	return err
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

func (d *DB) UpdateReviewGeneralComment(id int64, comment string) error {
	_, err := d.Exec("UPDATE reviews SET general_comment = ?, updated_at = datetime('now') WHERE id = ?", comment, id)
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

func (d *DB) CountReviewsSince(since time.Time) (int, error) {
	var count int
	err := d.QueryRow("SELECT COUNT(*) FROM reviews WHERE created_at >= ? AND deleted_at IS NULL", since.Format("2006-01-02 15:04:05")).Scan(&count)
	return count, err
}

func (d *DB) CountPublishedReviewsSince(since time.Time) (int, error) {
	var count int
	err := d.QueryRow("SELECT COUNT(*) FROM reviews WHERE published = 1 AND created_at >= ? AND deleted_at IS NULL", since.Format("2006-01-02 15:04:05")).Scan(&count)
	return count, err
}

// CreateExternalReview records a review that was performed outside this tool.
// It creates a synthetic ReviewRequest with status=done and a Review with
// outcome=reviewed_externally, published=true, linked to that request.
func (d *DB) CreateExternalReview(prID int64, commitSHA string) (int64, error) {
	tx, err := d.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	// Insert synthetic review request with status=done
	res, err := tx.Exec(
		`INSERT INTO review_requests (pull_request_id, status, created_at, updated_at) VALUES (?, 'done', ?, ?)`,
		prID, now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert review_request: %w", err)
	}
	reqID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id review_request: %w", err)
	}

	// Insert review with outcome=reviewed_externally, published=true
	res, err = tx.Exec(
		`INSERT INTO reviews (pull_request_id, review_request_id, outcome, commit_sha, summary, general_comment, published, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		prID, reqID, ReviewOutcomeReviewedExternally, commitSHA, "Reviewed externally", "", now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert review: %w", err)
	}
	reviewID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id review: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}

	return reviewID, nil
}

// HasExternalReview checks whether a review with outcome=reviewed_externally
// already exists for the given PR and commit SHA.
func (d *DB) HasExternalReview(prID int64, commitSHA string) (bool, error) {
	var count int
	err := d.QueryRow(
		`SELECT COUNT(*) FROM reviews WHERE pull_request_id = ? AND commit_sha = ? AND outcome = ? AND deleted_at IS NULL`,
		prID, commitSHA, ReviewOutcomeReviewedExternally,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check external review: %w", err)
	}
	return count > 0, nil
}
