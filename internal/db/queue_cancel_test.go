package db

import (
	"testing"
)

func TestSoftDeleteReviewRequestRemovesFromQueue(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "org/a", PRNumber: 1, Title: "t", Author: "a",
		CommitSHA: "abc", State: PRStateOpen, NeedsReview: true,
	})
	rrID, err := d.CreateReviewRequest(prID)
	if err != nil {
		t.Fatal(err)
	}

	if err := d.SoftDeleteReviewRequest(rrID); err != nil {
		t.Fatal(err)
	}

	list, err := d.ListReviewRequests()
	if err != nil {
		t.Fatal(err)
	}
	for _, rr := range list {
		if rr.ID == rrID {
			t.Fatalf("soft-deleted request still listed: %+v", rr)
		}
	}

	pr, err := d.GetPR(prID)
	if err != nil {
		t.Fatal(err)
	}
	if pr == nil || !pr.NeedsReview {
		t.Fatalf("needs_review should stay true, got %+v", pr)
	}
}

func TestSoftDeleteReviewRequestMissing(t *testing.T) {
	d := openTestDB(t)
	err := d.SoftDeleteReviewRequest(99999)
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSoftDeleteReviewRequestAlreadyDone(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "org/a", PRNumber: 2, Title: "t", Author: "a",
		CommitSHA: "abc", State: PRStateOpen, NeedsReview: true,
	})
	rrID, err := d.CreateReviewRequest(prID)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateReviewRequestStatus(rrID, ReviewRequestStatusDone); err != nil {
		t.Fatal(err)
	}
	if err := d.SoftDeleteReviewRequest(rrID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for done request, got %v", err)
	}
}

func TestGetReviewRequest(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "org/a", PRNumber: 3, Title: "t", Author: "a",
		CommitSHA: "abc", State: PRStateOpen, NeedsReview: true,
	})
	rrID, err := d.CreateReviewRequest(prID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetReviewRequest(rrID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.ID != rrID || got.Status != ReviewRequestStatusPending {
		t.Fatalf("unexpected request: %+v", got)
	}

	missing, err := d.GetReviewRequest(99999)
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatalf("expected nil, got %+v", missing)
	}
}
