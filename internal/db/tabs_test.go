package db

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func mustUpsert(t *testing.T, d *DB, pr PullRequest) int64 {
	t.Helper()
	id, err := d.UpsertPR(pr)
	if err != nil {
		t.Fatalf("UpsertPR: %v", err)
	}
	return id
}

func idsOf(prs []PullRequest) map[int64]bool {
	m := map[int64]bool{}
	for _, pr := range prs {
		m[pr.ID] = true
	}
	return m
}

func TestPRTabQueriesMutuallyExclusive(t *testing.T) {
	d := openTestDB(t)

	dashID := mustUpsert(t, d, PullRequest{
		Repo: "org/a", PRNumber: 1, Title: "needs review", Author: "alice",
		CommitSHA: "aaa", State: "open", NeedsReview: true,
	})
	histReviewedID := mustUpsert(t, d, PullRequest{
		Repo: "org/b", PRNumber: 2, Title: "reviewed open", Author: "bob",
		CommitSHA: "bbb", State: "open", NeedsReview: false,
	})
	filteredDraftID := mustUpsert(t, d, PullRequest{
		Repo: "org/c", PRNumber: 3, Title: "draft", Author: "carol",
		CommitSHA: "ccc", State: "open", NeedsReview: false, FilteredReason: "draft",
	})
	closedID := mustUpsert(t, d, PullRequest{
		Repo: "org/d", PRNumber: 4, Title: "merged", Author: "dave",
		CommitSHA: "ddd", State: "closed", NeedsReview: false,
	})
	closedWasFilteredID := mustUpsert(t, d, PullRequest{
		Repo: "org/e", PRNumber: 5, Title: "was draft then closed", Author: "eve",
		CommitSHA: "eee", State: "closed", NeedsReview: false, FilteredReason: "draft",
	})
	_ = mustUpsert(t, d, PullRequest{
		Repo: "org/f", PRNumber: 6, Title: "outdated limbo", Author: "frank",
		CommitSHA: "fff", State: "open", NeedsReview: true, IsOutdated: true,
	})

	dashboard, err := d.ListPRsNeedingReview()
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := d.ListFilteredPRs()
	if err != nil {
		t.Fatal(err)
	}
	history, err := d.ListHistoryPRs()
	if err != nil {
		t.Fatal(err)
	}

	dIDs, fIDs, hIDs := idsOf(dashboard), idsOf(filtered), idsOf(history)

	if !dIDs[dashID] {
		t.Errorf("dashboard missing open needs-review PR %d", dashID)
	}
	if fIDs[dashID] || hIDs[dashID] {
		t.Errorf("dashboard PR %d leaked into filtered=%v history=%v", dashID, fIDs[dashID], hIDs[dashID])
	}

	if !fIDs[filteredDraftID] {
		t.Errorf("filtered missing draft PR %d", filteredDraftID)
	}
	if dIDs[filteredDraftID] || hIDs[filteredDraftID] {
		t.Errorf("filtered PR %d leaked into dashboard=%v history=%v", filteredDraftID, dIDs[filteredDraftID], hIDs[filteredDraftID])
	}

	if !hIDs[histReviewedID] {
		t.Errorf("history missing reviewed-open PR %d", histReviewedID)
	}
	if !hIDs[closedID] {
		t.Errorf("history missing closed PR %d", closedID)
	}
	if !hIDs[closedWasFilteredID] {
		t.Errorf("history missing closed-ex-filtered PR %d", closedWasFilteredID)
	}
	if fIDs[closedID] || fIDs[closedWasFilteredID] {
		t.Errorf("closed PRs must not appear on filtered: closed=%v closedWasFiltered=%v", fIDs[closedID], fIDs[closedWasFilteredID])
	}

	// Pairwise disjointness across all returned IDs.
	for id := range dIDs {
		if fIDs[id] || hIDs[id] {
			t.Errorf("id %d on dashboard and another tab", id)
		}
	}
	for id := range fIDs {
		if hIDs[id] {
			t.Errorf("id %d on both filtered and history", id)
		}
	}
}

func TestMigrateClearsStateFilteredReason(t *testing.T) {
	d := openTestDB(t)
	id := mustUpsert(t, d, PullRequest{
		Repo: "org/x", PRNumber: 99, Title: "bad tag", Author: "x",
		CommitSHA: "x", State: "closed", NeedsReview: false, FilteredReason: "state",
	})
	if _, err := d.Exec("UPDATE pull_requests SET filtered_reason = 'state' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	// Simulate migrate cleanup SQL
	if _, err := d.Exec("UPDATE pull_requests SET filtered_reason = NULL WHERE filtered_reason = 'state'"); err != nil {
		t.Fatal(err)
	}
	pr, err := d.GetPR(id)
	if err != nil {
		t.Fatal(err)
	}
	if pr.FilteredReason != "" {
		t.Fatalf("expected cleared filtered_reason, got %q", pr.FilteredReason)
	}
	filtered, err := d.ListFilteredPRs()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range filtered {
		if p.ID == id {
			t.Fatal("closed PR with cleared reason still on filtered")
		}
	}
	history, err := d.ListHistoryPRs()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range history {
		if p.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("closed PR should be on history after clearing state reason")
	}
}
