package db

import (
	"testing"
	"time"
)

func TestReviewCommentPublishedRoundTrip(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{Repo: "o/r", PRNumber: 1, Title: "t", Author: "a", CommitSHA: "1", State: PRStateOpen})
	reviewID := mustReviewAt(t, d, prID, ReviewOutcomeHumanReview, time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC))

	id, err := d.AddReviewComment(ReviewComment{ReviewID: reviewID, File: "a.go", Line: 10, Message: "fix me"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := d.GetReviewComment(id)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected comment")
	}
	if got.Published {
		t.Fatalf("new comment should be unpublished, got %+v", got)
	}

	if err := d.PublishReviewComment(id); err != nil {
		t.Fatal(err)
	}

	got, err = d.GetReviewComment(id)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Published {
		t.Fatalf("expected published, got %+v", got)
	}

	list, err := d.ListReviewComments(reviewID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Published {
		t.Fatalf("list published: %+v", list)
	}
}

func TestGetReviewCommentMissing(t *testing.T) {
	d := openTestDB(t)
	got, err := d.GetReviewComment(99999)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}
