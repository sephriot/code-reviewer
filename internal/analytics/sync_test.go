package analytics

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/db"
	gh "github.com/sephriot/code-reviewer/internal/github"
)

type fakeGitHub struct {
	pr           gh.TimingCandidate
	first        *time.Time
	calls        int
	reviewCalls  int
	failAt       int
	updatedSince *time.Time
}

func (f *fakeGitHub) WalkTimingPRs(ctx context.Context, direction string, since, before time.Time, updated *time.Time, visit func(gh.TimingCandidate) error) error {
	f.calls++
	if f.failAt == f.calls {
		return errors.New("rate limited")
	}
	if updated != nil {
		t := *updated
		f.updatedSince = &t
	}
	if !f.pr.CreatedAt.Before(since) && f.pr.CreatedAt.Before(before) && (updated == nil || !f.pr.UpdatedAt.Before(*updated)) {
		return visit(f.pr)
	}
	return ctx.Err()
}
func (f *fakeGitHub) FirstReviewTime(context.Context, gh.TimingCandidate, string) (*time.Time, error) {
	f.reviewCalls++
	return f.first, nil
}

func TestSyncResumesBackfillAndRefreshesDaily(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "timing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	first := now.Add(-24 * time.Hour)
	client := &fakeGitHub{pr: gh.TimingCandidate{Owner: "o", Repo: "r", Number: 1, Author: "author", CreatedAt: first.Add(-2 * time.Hour), UpdatedAt: now}, first: &first, failAt: 3}
	syncer := New(d, client, "me")
	if err := syncer.Sync(context.Background(), now); err == nil {
		t.Fatal("expected interrupted backfill")
	}
	state, err := d.GetTimingSyncState("me")
	if err != nil {
		t.Fatal(err)
	}
	wantBefore := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	if !state.BackfillBefore.Equal(wantBefore) || state.LastError != "rate limited" || state.LastSyncedAt != nil {
		t.Fatalf("partial state: %+v", state)
	}
	if err := syncer.Sync(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	state, err = d.GetTimingSyncState("me")
	if err != nil {
		t.Fatal(err)
	}
	if state.BackfillBefore.After(state.BackfillSince) || state.LastSyncedAt == nil || state.LastError != "" {
		t.Fatalf("complete state: %+v", state)
	}
	if client.reviewCalls != 2 {
		t.Fatalf("refetched cached reviews %d times", client.reviewCalls)
	}
	calls := client.calls
	if err := syncer.Sync(context.Background(), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if client.calls != calls {
		t.Fatal("synced more than once daily")
	}
	tomorrow := now.Add(24 * time.Hour)
	client.pr.UpdatedAt = tomorrow
	if err := syncer.Sync(context.Background(), tomorrow); err != nil {
		t.Fatal(err)
	}
	if client.updatedSince == nil || !client.updatedSince.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("missing overlapping incremental cutoff: %v", client.updatedSince)
	}
	if client.reviewCalls != 4 {
		t.Fatalf("did not refresh changed PR: %d calls", client.reviewCalls)
	}
	stats, err := d.ReviewTimingAnalytics("me", now.AddDate(0, 0, -365), tomorrow)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Given.Count != 1 || stats.Received.Count != 1 {
		t.Fatalf("duplicated samples: %+v", stats)
	}
	var queueCount int
	if err := d.QueryRow(`SELECT COUNT(*) FROM pull_requests`).Scan(&queueCount); err != nil {
		t.Fatal(err)
	}
	if queueCount != 0 {
		t.Fatal("timing import populated the review queue")
	}
}

func TestSyncKeepsMissingReceivedReviewUntilLaterRefresh(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "timing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	state, err := d.TimingSyncState("me", now)
	if err != nil {
		t.Fatal(err)
	}
	state.BackfillBefore = state.BackfillSince
	state.LastSyncedAt = &now
	if err := d.SaveTimingSyncState("me", state); err != nil {
		t.Fatal(err)
	}
	opened := now.Add(-time.Hour)
	client := &fakeReceivedGitHub{pr: gh.TimingCandidate{Owner: "o", Repo: "r", Number: 1, Author: "me", CreatedAt: opened, UpdatedAt: now}}
	syncer := New(d, client, "me")
	tomorrow := now.Add(24 * time.Hour)
	if err := syncer.Sync(context.Background(), tomorrow); err != nil {
		t.Fatal(err)
	}
	stats, err := d.ReviewTimingAnalytics("me", now.AddDate(0, 0, -7), tomorrow)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Received.Awaiting != 1 || stats.Received.Count != 0 {
		t.Fatalf("missing-review stats: %+v", stats.Received)
	}
	client.first = &tomorrow
	client.pr.UpdatedAt = tomorrow
	if err := syncer.Sync(context.Background(), tomorrow.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stats, err = d.ReviewTimingAnalytics("me", now.AddDate(0, 0, -7), tomorrow.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Received.Awaiting != 0 || stats.Received.Count != 1 || *stats.Received.MedianHours != 25 {
		t.Fatalf("received stats: %+v", stats.Received)
	}
}

type fakeReceivedGitHub struct {
	pr    gh.TimingCandidate
	first *time.Time
}

func (f *fakeReceivedGitHub) WalkTimingPRs(ctx context.Context, direction string, since, before time.Time, updated *time.Time, visit func(gh.TimingCandidate) error) error {
	if direction == "received" {
		return visit(f.pr)
	}
	return nil
}
func (f *fakeReceivedGitHub) FirstReviewTime(context.Context, gh.TimingCandidate, string) (*time.Time, error) {
	return f.first, nil
}
