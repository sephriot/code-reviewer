package db

import (
	"testing"
	"time"
)

func TestReviewTimingDistributionAndDailyTrend(t *testing.T) {
	d := openTestDB(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	if _, err := d.TimingSyncState("Me", now); err != nil {
		t.Fatal(err)
	}
	reviewed := now.Add(-24 * time.Hour)
	for i, hours := range []float64{0, 1, 4, 12, 24, 48, 168, 200} {
		sample := ReviewTiming{Repo: "org/repo", PRNumber: i + 1, Direction: "given", OpenedAt: reviewed.Add(-time.Duration(hours * float64(time.Hour))), ReviewedAt: &reviewed, UpdatedAt: now}
		if err := d.UpsertReviewTiming("ME", sample); err != nil {
			t.Fatal(err)
		}
		if err := d.UpsertReviewTiming("me", sample); err != nil {
			t.Fatal(err)
		}
	}
	sample := ReviewTiming{Repo: "org/repo", PRNumber: 1, Direction: "received", OpenedAt: reviewed.Add(-2 * time.Hour), ReviewedAt: &reviewed, UpdatedAt: now}
	if err := d.UpsertReviewTiming("me", sample); err != nil {
		t.Fatal(err)
	}
	sample.PRNumber, sample.ReviewedAt = 2, nil
	if err := d.UpsertReviewTiming("me", sample); err != nil {
		t.Fatal(err)
	}
	sample.PRNumber, sample.OpenedAt = 3, now.AddDate(0, 0, -100)
	if err := d.UpsertReviewTiming("me", sample); err != nil {
		t.Fatal(err)
	}
	sample.PRNumber, sample.OpenedAt, sample.ReviewedAt = 4, now.Add(-time.Hour), &now
	if err := d.UpsertReviewTiming("other", sample); err != nil {
		t.Fatal(err)
	}
	since := now.AddDate(0, 0, -3)
	got, err := d.ReviewTimingAnalytics("mE", since, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Given.Count != 8 || got.Received.Count != 1 || got.Received.Awaiting != 1 {
		t.Fatalf("counts: %+v", got)
	}
	for i, bucket := range got.Given.Histogram {
		want := 1
		if i == 6 {
			want = 2
		}
		if bucket.Count != want {
			t.Errorf("bucket %s = %d, want %d", bucket.Label, bucket.Count, want)
		}
	}
	if *got.Given.MedianHours != 18 || *got.Given.AverageHours != 57.125 {
		t.Fatalf("summary: %+v", got.Given)
	}
	if len(got.Given.Daily) != 4 || got.Given.Daily[2].Count != 8 || got.Given.Daily[0].MedianHours != nil {
		t.Fatalf("daily: %+v", got.Given.Daily)
	}
	if got.Received.Daily[2].Count != 1 || *got.Received.Daily[2].MedianHours != 2 {
		t.Fatalf("received daily: %+v", got.Received.Daily)
	}
	// A later refresh replaces the previously missing review instead of adding a second PR.
	sample = ReviewTiming{Repo: "ORG/REPO", PRNumber: 2, Direction: "received", OpenedAt: reviewed.Add(-time.Hour), ReviewedAt: &reviewed, UpdatedAt: now}
	if err := d.UpsertReviewTiming("me", sample); err != nil {
		t.Fatal(err)
	}
	got, err = d.ReviewTimingAnalytics("me", since, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Received.Count != 2 || got.Received.Awaiting != 0 || *got.Received.MedianHours != 1.5 {
		t.Fatalf("updated stats: %+v", got.Received)
	}
}

func TestTimingSyncStateSurvivesReopenAndIsPerUser(t *testing.T) {
	path := t.TempDir() + "/timing.db"
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	state, err := d.TimingSyncState("ME", now)
	if err != nil {
		t.Fatal(err)
	}
	wantSince := time.Date(2025, 10, 19, 0, 0, 0, 0, time.UTC)
	if state.BackfillSince != wantSince || state.BackfillBefore.Day() != 20 {
		t.Fatalf("initial state: %+v", state)
	}
	state.BackfillBefore = state.BackfillBefore.AddDate(0, 0, -30)
	state.LastError = "rate limited"
	if err := d.SaveTimingSyncState("me", state); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	got, err := d.TimingSyncState("Me", now.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !got.BackfillBefore.Equal(state.BackfillBefore) || got.LastError != state.LastError || !got.BackfillSince.Equal(wantSince) {
		t.Fatalf("resumed: %+v", got)
	}
	other, err := d.GetTimingSyncState("other")
	if err != nil || !other.BackfillSince.IsZero() {
		t.Fatalf("other state: %+v, %v", other, err)
	}
}

func TestReviewTimingRejectsNegativeDuration(t *testing.T) {
	d := openTestDB(t)
	now := time.Now()
	reviewed := now.Add(-time.Hour)
	err := d.UpsertReviewTiming("me", ReviewTiming{Repo: "a/b", PRNumber: 1, Direction: "received", OpenedAt: now, ReviewedAt: &reviewed, UpdatedAt: now})
	if err == nil {
		t.Fatal("accepted a review before PR creation")
	}
}
