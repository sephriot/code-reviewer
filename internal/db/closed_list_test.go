package db

import (
	"path/filepath"
	"testing"
)

func TestListClosedPRsOnlyClosedNotMergedOrHistoryOpen(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	if _, err := d.UpsertPR(PullRequest{Repo: "o/r", PRNumber: 1, Title: "c", Author: "a", CommitSHA: "1", State: PRStateClosed}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertPR(PullRequest{Repo: "o/r", PRNumber: 2, Title: "m", Author: "a", CommitSHA: "2", State: PRStateMerged}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertPR(PullRequest{Repo: "o/r", PRNumber: 3, Title: "o", Author: "a", CommitSHA: "3", State: PRStateOpen, NeedsReview: false}); err != nil {
		t.Fatal(err)
	}

	closed, err := d.ListClosedPRs()
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[0].PRNumber != 1 {
		t.Fatalf("got %#v", closed)
	}
}
