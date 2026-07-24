package db

import (
	"testing"
	"time"
)

func mustReviewAt(t *testing.T, d *DB, prID int64, outcome string, createdAt time.Time) int64 {
	t.Helper()
	rrID, err := d.CreateReviewRequest(prID)
	if err != nil {
		t.Fatalf("CreateReviewRequest: %v", err)
	}
	id, err := d.CreateReview(Review{
		PullRequestID:   prID,
		ReviewRequestID: rrID,
		Outcome:         outcome,
		CommitSHA:       "abc",
		Summary:         "s",
	})
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	ts := createdAt.UTC().Format("2006-01-02 15:04:05")
	if _, err := d.Exec(`UPDATE reviews SET created_at = ?, updated_at = ? WHERE id = ?`, ts, ts, id); err != nil {
		t.Fatalf("backdate review: %v", err)
	}
	return id
}

func TestSqliteYearWeekMatchesSQLite(t *testing.T) {
	d := openTestDB(t)
	dates := []string{
		"2026-01-01", // Thu before first Monday (Jan 5) → 2026-00
		"2026-01-05", // first Monday → 2026-01
		"2026-01-11", // Sunday of week 01 → 2026-01
		"2026-01-12", // Monday week 02 → 2026-02
		"2026-07-24",
		"2025-12-31",
	}
	for _, ds := range dates {
		var want string
		if err := d.QueryRow(`SELECT strftime('%Y-%W', ?)`, ds).Scan(&want); err != nil {
			t.Fatalf("sqlite strftime: %v", err)
		}
		parsed, err := time.ParseInLocation("2006-01-02", ds, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		got := sqliteYearWeek(parsed)
		if got != want {
			t.Errorf("%s: got %q want %q", ds, got, want)
		}
	}
}

func TestReviewsByOutcomeOverTimeDayGrouping(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "o/r", PRNumber: 1, Title: "t", Author: "a", CommitSHA: "1", State: PRStateOpen,
	})

	day1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 7, 2, 8, 0, 0, 0, time.UTC)
	mustReviewAt(t, d, prID, ReviewOutcomeApproveWithComments, day1)
	mustReviewAt(t, d, prID, ReviewOutcomeHumanReview, day1)
	mustReviewAt(t, d, prID, ReviewOutcomeApproveWithoutComments, day2)

	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	rows, err := d.ReviewsByOutcomeOverTime(since, TrendBucketDay)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]map[string]int{}
	for _, r := range rows {
		if got[r.Bucket] == nil {
			got[r.Bucket] = map[string]int{}
		}
		got[r.Bucket][r.Outcome] = r.Count
	}
	if got["2026-07-01"][ReviewOutcomeApproveWithComments] != 1 {
		t.Fatalf("day1 approve_with: %#v", got["2026-07-01"])
	}
	if got["2026-07-01"][ReviewOutcomeHumanReview] != 1 {
		t.Fatalf("day1 human: %#v", got["2026-07-01"])
	}
	if got["2026-07-02"][ReviewOutcomeApproveWithoutComments] != 1 {
		t.Fatalf("day2: %#v", got["2026-07-02"])
	}
}

func TestReviewsByOutcomeOverTimeExcludesSoftDeleted(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "o/r", PRNumber: 2, Title: "t", Author: "a", CommitSHA: "2", State: PRStateOpen,
	})
	at := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	id := mustReviewAt(t, d, prID, ReviewOutcomeToolFailed, at)
	if _, err := d.Exec(`UPDATE reviews SET deleted_at = ? WHERE id = ?`, at.Format("2006-01-02 15:04:05"), id); err != nil {
		t.Fatal(err)
	}
	mustReviewAt(t, d, prID, ReviewOutcomeChangesRequested, at)

	rows, err := d.ReviewsByOutcomeOverTime(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), TrendBucketDay)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Outcome != ReviewOutcomeChangesRequested || rows[0].Count != 1 {
		t.Fatalf("got %#v", rows)
	}
}

func TestReviewsByOutcomeOverTimeWeekGrouping(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "o/r", PRNumber: 3, Title: "t", Author: "a", CommitSHA: "3", State: PRStateOpen,
	})
	// 2026-01-05 is first Monday → week 01; 2026-01-12 is week 02
	mustReviewAt(t, d, prID, ReviewOutcomeApproveWithComments, time.Date(2026, 1, 6, 12, 0, 0, 0, time.UTC))
	mustReviewAt(t, d, prID, ReviewOutcomeHumanReview, time.Date(2026, 1, 13, 12, 0, 0, 0, time.UTC))

	rows, err := d.ReviewsByOutcomeOverTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), TrendBucketWeek)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, r := range rows {
		got[r.Bucket+"|"+r.Outcome] = r.Count
	}
	if got["2026-01|"+ReviewOutcomeApproveWithComments] != 1 {
		t.Fatalf("week01: %#v", got)
	}
	if got["2026-02|"+ReviewOutcomeHumanReview] != 1 {
		t.Fatalf("week02: %#v", got)
	}
}

func TestFillTrendBucketsZeroFillsDays(t *testing.T) {
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	rows := []OutcomeCountRow{
		{Bucket: "2026-07-01", Outcome: ReviewOutcomeHumanReview, Count: 2},
		{Bucket: "2026-07-03", Outcome: ReviewOutcomeToolFailed, Count: 1},
	}
	filled := FillTrendBuckets(since, until, TrendBucketDay, rows)
	if len(filled) != 3 {
		t.Fatalf("len=%d %#v", len(filled), filled)
	}
	if filled[0].Date != "2026-07-01" || filled[0].Total != 2 {
		t.Fatalf("day0 %#v", filled[0])
	}
	if filled[1].Date != "2026-07-02" || filled[1].Total != 0 || len(filled[1].Outcomes) != 0 {
		t.Fatalf("day1 gap %#v", filled[1])
	}
	if filled[2].Date != "2026-07-03" || filled[2].Total != 1 {
		t.Fatalf("day2 %#v", filled[2])
	}
}

func TestFillTrendBucketsEmptyWindow(t *testing.T) {
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	filled := FillTrendBuckets(since, until, TrendBucketDay, nil)
	if len(filled) != 2 || filled[0].Total != 0 || filled[1].Total != 0 {
		t.Fatalf("%#v", filled)
	}
	if filled[0].Date == "" || filled[0].Week != "" {
		t.Fatalf("expected date key only: %#v", filled[0])
	}
}

func TestFillTrendBucketsWeekKeys(t *testing.T) {
	since := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)  // Monday week 01
	until := time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC) // Monday week 02
	filled := FillTrendBuckets(since, until, TrendBucketWeek, []OutcomeCountRow{
		{Bucket: "2026-01", Outcome: ReviewOutcomeApproveWithComments, Count: 3},
	})
	if len(filled) != 2 {
		t.Fatalf("len=%d %#v", len(filled), filled)
	}
	if filled[0].Week != "2026-01" || filled[0].Total != 3 {
		t.Fatalf("w0 %#v", filled[0])
	}
	if filled[1].Week != "2026-02" || filled[1].Total != 0 {
		t.Fatalf("w1 %#v", filled[1])
	}
}

func TestReviewsByAuthorStatsRankingAndRates(t *testing.T) {
	d := openTestDB(t)
	alice := mustUpsert(t, d, PullRequest{Repo: "o/r", PRNumber: 1, Title: "t", Author: "alice", CommitSHA: "1", State: PRStateOpen})
	bob := mustUpsert(t, d, PullRequest{Repo: "o/r", PRNumber: 2, Title: "t", Author: "bob", CommitSHA: "2", State: PRStateOpen})
	at := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	// alice: 2 approve_with, 1 human → total 3, approval 66.7, human 33.3, changes 0
	mustReviewAt(t, d, alice, ReviewOutcomeApproveWithComments, at)
	mustReviewAt(t, d, alice, ReviewOutcomeApproveWithComments, at)
	mustReviewAt(t, d, alice, ReviewOutcomeHumanReview, at)
	// bob: 1 changes → total 1, approval 0, human 0, changes 100
	mustReviewAt(t, d, bob, ReviewOutcomeChangesRequested, at)

	got, err := d.ReviewsByAuthorStats(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 15)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d %#v", len(got), got)
	}
	if got[0].Author != "alice" || got[0].TotalReviews != 3 {
		t.Fatalf("rank0 %#v", got[0])
	}
	if got[0].ApprovalRate != 66.7 || got[0].HumanReviewRate != 33.3 || got[0].ChangeRequestRate != 0 {
		t.Fatalf("alice rates %#v", got[0])
	}
	if got[1].Author != "bob" || got[1].TotalReviews != 1 || got[1].ChangeRequestRate != 100 {
		t.Fatalf("bob %#v", got[1])
	}
}

func TestReviewsByAuthorStatsExcludesSoftDeletedAndBeforeSince(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{Repo: "o/r", PRNumber: 3, Title: "t", Author: "carol", CommitSHA: "3", State: PRStateOpen})
	inWindow := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	old := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	keep := mustReviewAt(t, d, prID, ReviewOutcomeApproveWithoutComments, inWindow)
	deleted := mustReviewAt(t, d, prID, ReviewOutcomeHumanReview, inWindow)
	mustReviewAt(t, d, prID, ReviewOutcomeApproveWithComments, old)
	ts := inWindow.Format("2006-01-02 15:04:05")
	if _, err := d.Exec(`UPDATE reviews SET deleted_at = ? WHERE id = ?`, ts, deleted); err != nil {
		t.Fatal(err)
	}
	_ = keep

	got, err := d.ReviewsByAuthorStats(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 15)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Author != "carol" || got[0].TotalReviews != 1 || got[0].ApprovalRate != 100 {
		t.Fatalf("%#v", got)
	}
}

func TestReviewsByAuthorStatsAvgCommentsAndLimit(t *testing.T) {
	d := openTestDB(t)
	a := mustUpsert(t, d, PullRequest{Repo: "o/r", PRNumber: 10, Title: "t", Author: "a", CommitSHA: "a", State: PRStateOpen})
	b := mustUpsert(t, d, PullRequest{Repo: "o/r", PRNumber: 11, Title: "t", Author: "b", CommitSHA: "b", State: PRStateOpen})
	c := mustUpsert(t, d, PullRequest{Repo: "o/r", PRNumber: 12, Title: "t", Author: "c", CommitSHA: "c", State: PRStateOpen})
	at := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	r1 := mustReviewAt(t, d, a, ReviewOutcomeApproveWithComments, at)
	r2 := mustReviewAt(t, d, a, ReviewOutcomeApproveWithComments, at)
	if _, err := d.AddReviewComment(ReviewComment{ReviewID: r1, File: "f.go", Line: 1, Message: "m1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddReviewComment(ReviewComment{ReviewID: r1, File: "f.go", Line: 2, Message: "m2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddReviewComment(ReviewComment{ReviewID: r2, File: "f.go", Line: 3, Message: "m3"}); err != nil {
		t.Fatal(err)
	}
	// soft-deleted comment must not count
	cid, err := d.AddReviewComment(ReviewComment{ReviewID: r2, File: "f.go", Line: 4, Message: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteReviewComment(cid); err != nil {
		t.Fatal(err)
	}

	mustReviewAt(t, d, b, ReviewOutcomeHumanReview, at)
	mustReviewAt(t, d, c, ReviewOutcomeToolFailed, at)

	got, err := d.ReviewsByAuthorStats(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("limit: %#v", got)
	}
	if got[0].Author != "a" || got[0].AvgInlineComments != 1.5 {
		// (2 + 1) / 2 reviews = 1.5
		t.Fatalf("avg comments %#v", got[0])
	}
}
