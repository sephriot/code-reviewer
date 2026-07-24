package db

import (
	"fmt"
	"math"
	"time"
)

// ReviewsByOutcomeOverTime returns sparse counts grouped by day or week bucket and outcome.
// Soft-deleted reviews are excluded. Bucket must be TrendBucketDay or TrendBucketWeek.
func (d *DB) ReviewsByOutcomeOverTime(since time.Time, bucket string) ([]OutcomeCountRow, error) {
	var groupExpr string
	switch bucket {
	case TrendBucketWeek:
		groupExpr = "strftime('%Y-%W', created_at)"
	case TrendBucketDay, "":
		groupExpr = "DATE(created_at)"
		bucket = TrendBucketDay
	default:
		return nil, fmt.Errorf("unknown trend bucket %q", bucket)
	}

	q := fmt.Sprintf(`
SELECT %s AS bucket, outcome, COUNT(*) AS cnt
FROM reviews
WHERE created_at >= ? AND deleted_at IS NULL
GROUP BY bucket, outcome
ORDER BY bucket ASC`, groupExpr)

	rows, err := d.Query(q, since.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OutcomeCountRow
	for rows.Next() {
		var r OutcomeCountRow
		if err := rows.Scan(&r.Bucket, &r.Outcome, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReviewsByAuthorStats returns top authors by review count since the given time.
// Soft-deleted reviews are excluded. Rates are percentages to 1 decimal place.
// AvgInlineComments averages non-deleted review_comments per review.
func (d *DB) ReviewsByAuthorStats(since time.Time, limit int) ([]AuthorStats, error) {
	if limit <= 0 {
		limit = 15
	}
	q := `
SELECT
	p.author,
	COUNT(*) AS total_reviews,
	SUM(CASE WHEN r.outcome = ? THEN 1 ELSE 0 END) AS approve_with,
	SUM(CASE WHEN r.outcome = ? THEN 1 ELSE 0 END) AS approve_without,
	SUM(CASE WHEN r.outcome = ? THEN 1 ELSE 0 END) AS changes_requested,
	SUM(CASE WHEN r.outcome = ? THEN 1 ELSE 0 END) AS human_review,
	AVG((
		SELECT COUNT(*) FROM review_comments c
		WHERE c.review_id = r.id AND c.deleted_at IS NULL
	)) AS avg_comments
FROM reviews r
JOIN pull_requests p ON r.pull_request_id = p.id
WHERE r.created_at >= ? AND r.deleted_at IS NULL
	AND p.author IS NOT NULL AND p.author != ''
GROUP BY p.author
ORDER BY total_reviews DESC
LIMIT ?`
	rows, err := d.Query(
		q,
		ReviewOutcomeApproveWithComments,
		ReviewOutcomeApproveWithoutComments,
		ReviewOutcomeChangesRequested,
		ReviewOutcomeHumanReview,
		since.UTC().Format("2006-01-02 15:04:05"),
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuthorStats
	for rows.Next() {
		var (
			author                             string
			total, approveWith, approveWithout int
			changesRequested, humanReview      int
			avgComments                        float64
		)
		if err := rows.Scan(
			&author, &total, &approveWith, &approveWithout,
			&changesRequested, &humanReview, &avgComments,
		); err != nil {
			return nil, err
		}
		s := AuthorStats{
			Author:            author,
			TotalReviews:      total,
			AvgInlineComments: round1(avgComments),
		}
		if total > 0 {
			s.ApprovalRate = round1(float64(approveWith+approveWithout) / float64(total) * 100)
			s.HumanReviewRate = round1(float64(humanReview) / float64(total) * 100)
			s.ChangeRequestRate = round1(float64(changesRequested) / float64(total) * 100)
		}
		out = append(out, s)
	}
	if out == nil {
		out = []AuthorStats{}
	}
	return out, rows.Err()
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

// FillTrendBuckets expands sparse OutcomeCountRow values into a continuous series
// from since through until (inclusive by calendar day). Missing buckets get total 0.
func FillTrendBuckets(since, until time.Time, bucket string, rows []OutcomeCountRow) []TrendBucket {
	byBucket := map[string]map[string]int{}
	for _, r := range rows {
		if byBucket[r.Bucket] == nil {
			byBucket[r.Bucket] = map[string]int{}
		}
		byBucket[r.Bucket][r.Outcome] += r.Count
	}

	keys := trendBucketKeys(since, until, bucket)
	out := make([]TrendBucket, 0, len(keys))
	for _, key := range keys {
		outcomes := byBucket[key]
		if outcomes == nil {
			outcomes = map[string]int{}
		}
		total := 0
		for _, n := range outcomes {
			total += n
		}
		tb := TrendBucket{Total: total, Outcomes: outcomes}
		if bucket == TrendBucketWeek {
			tb.Week = key
		} else {
			tb.Date = key
		}
		out = append(out, tb)
	}
	return out
}

func trendBucketKeys(since, until time.Time, bucket string) []string {
	start := truncateUTCDay(since)
	end := truncateUTCDay(until)
	if end.Before(start) {
		return nil
	}

	seen := map[string]bool{}
	var keys []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		var key string
		if bucket == TrendBucketWeek {
			key = sqliteYearWeek(d)
		} else {
			key = d.Format("2006-01-02")
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}

func truncateUTCDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// sqliteYearWeek matches SQLite strftime('%Y-%W', t): week 01 starts on the
// first Monday of the year; days before that are week 00.
func sqliteYearWeek(t time.Time) string {
	t = truncateUTCDay(t)
	year := t.Year()
	jan1 := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	// Weekday: Sunday=0 ... Saturday=6 (same as time.Weekday)
	wd := int(jan1.Weekday())
	var daysToMonday int
	switch wd {
	case int(time.Monday):
		daysToMonday = 0
	case int(time.Sunday):
		daysToMonday = 1
	default:
		daysToMonday = (8 - wd) % 7
	}
	firstMonday := jan1.AddDate(0, 0, daysToMonday)
	if t.Before(firstMonday) {
		return fmt.Sprintf("%d-00", year)
	}
	week := int(t.Sub(firstMonday).Hours()/24)/7 + 1
	return fmt.Sprintf("%d-%02d", year, week)
}
