package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUpsertPR_StoresAndPreservesGhUpdatedAt(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	ghAt := time.Date(2026, 7, 20, 15, 30, 0, 0, time.UTC)
	id, err := d.UpsertPR(PullRequest{
		Repo: "o/r", PRNumber: 1, Title: "a", Author: "a", CommitSHA: "c1",
		State: PRStateOpen, NeedsReview: true, GhUpdatedAt: ghAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := d.GetPR(id)
	if err != nil || pr == nil {
		t.Fatalf("get: %v %#v", err, pr)
	}
	if !pr.GhUpdatedAt.Equal(ghAt) {
		t.Fatalf("GhUpdatedAt=%v want %v", pr.GhUpdatedAt, ghAt)
	}

	_, err = d.UpsertPR(PullRequest{
		Repo: "o/r", PRNumber: 1, Title: "a2", Author: "a", CommitSHA: "c1",
		State: PRStateOpen, NeedsReview: true,
		// GhUpdatedAt zero — must preserve
	})
	if err != nil {
		t.Fatal(err)
	}
	pr, _ = d.GetPR(id)
	if !pr.GhUpdatedAt.Equal(ghAt) {
		t.Fatalf("preserved GhUpdatedAt=%v want %v", pr.GhUpdatedAt, ghAt)
	}

	newer := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	_, err = d.UpsertPR(PullRequest{
		Repo: "o/r", PRNumber: 1, Title: "a3", Author: "a", CommitSHA: "c2",
		State: PRStateOpen, NeedsReview: true, GhUpdatedAt: newer,
	})
	if err != nil {
		t.Fatal(err)
	}
	pr, _ = d.GetPR(id)
	if !pr.GhUpdatedAt.Equal(newer) {
		t.Fatalf("updated GhUpdatedAt=%v want %v", pr.GhUpdatedAt, newer)
	}
}

func TestListPRsNeedingReview_OrdersByGhUpdatedAtDesc(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	old := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	new_ := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)

	mustUpsert(t, d, PullRequest{
		Repo: "o/r", PRNumber: 1, Title: "old", Author: "a", CommitSHA: "1",
		State: PRStateOpen, NeedsReview: true, GhUpdatedAt: old,
	})
	mustUpsert(t, d, PullRequest{
		Repo: "o/r", PRNumber: 2, Title: "new", Author: "a", CommitSHA: "2",
		State: PRStateOpen, NeedsReview: true, GhUpdatedAt: new_,
	})
	mustUpsert(t, d, PullRequest{
		Repo: "o/r", PRNumber: 3, Title: "mid", Author: "a", CommitSHA: "3",
		State: PRStateOpen, NeedsReview: true, GhUpdatedAt: mid,
	})

	list, err := d.ListPRsNeedingReview()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("len=%d", len(list))
	}
	if list[0].PRNumber != 2 || list[1].PRNumber != 3 || list[2].PRNumber != 1 {
		t.Fatalf("order PRNumbers=%d,%d,%d want 2,3,1", list[0].PRNumber, list[1].PRNumber, list[2].PRNumber)
	}
}

func TestListFilteredPRs_OrdersByGhUpdatedAtDesc(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	old := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	new_ := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	mustUpsert(t, d, PullRequest{
		Repo: "o/r", PRNumber: 10, Title: "old", Author: "a", CommitSHA: "1",
		State: PRStateOpen, NeedsReview: false, FilteredReason: "draft", GhUpdatedAt: old,
	})
	mustUpsert(t, d, PullRequest{
		Repo: "o/r", PRNumber: 11, Title: "new", Author: "a", CommitSHA: "2",
		State: PRStateOpen, NeedsReview: false, FilteredReason: "author", GhUpdatedAt: new_,
	})

	list, err := d.ListFilteredPRs()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].PRNumber != 11 || list[1].PRNumber != 10 {
		t.Fatalf("got %#v", list)
	}
}
