package review

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
)

func openReactorDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestCancelRequestSoftDeletesPending(t *testing.T) {
	d := openReactorDB(t)
	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "o/r", PRNumber: 1, Title: "t", Author: "a",
		CommitSHA: "1", State: db.PRStateOpen, IsAssigned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rrID, err := d.CreateReviewRequest(prID, "1")
	if err != nil {
		t.Fatal(err)
	}

	r := NewReactor(&config.Config{}, d, nil, nil, nil)
	if err := r.CancelRequest(rrID); err != nil {
		t.Fatal(err)
	}

	list, err := d.ListReviewRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty queue, got %+v", list)
	}
	request, err := d.GetReviewRequestIncludingTerminal(rrID)
	if err != nil {
		t.Fatal(err)
	}
	if request == nil || request.Status != db.ReviewRequestStatusSuppressed {
		t.Fatalf("user cancellation = %#v, want suppressed", request)
	}
}

func TestProcessQueueCancelLeavesNoReview(t *testing.T) {
	d := openReactorDB(t)
	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "o/r", PRNumber: 2, Title: "t", Author: "a",
		CommitSHA: "2", State: db.PRStateOpen, IsAssigned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rrID, err := d.CreateReviewRequest(prID, "2")
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	var events []ReviewEvent
	var eventsMu sync.Mutex

	r := NewReactor(&config.Config{ReviewTimeout: time.Minute}, d, nil, nil, func(e ReviewEvent) {
		eventsMu.Lock()
		events = append(events, e)
		eventsMu.Unlock()
	})
	r.runReview = func(ctx context.Context, pr db.PullRequest, promptPath string) (*ReviewResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	done := make(chan error, 1)
	go func() {
		done <- r.ProcessQueue(context.Background())
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("review did not start")
	}

	if err := r.CancelRequest(rrID); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessQueue: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ProcessQueue did not finish after cancel")
	}

	reviews, err := d.ListReviewsForPR(prID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 0 {
		t.Fatalf("expected no reviews, got %+v", reviews)
	}
	request, err := d.GetReviewRequestIncludingTerminal(rrID)
	if err != nil {
		t.Fatal(err)
	}
	if request == nil || request.Status != db.ReviewRequestStatusSuppressed {
		t.Fatalf("user cancellation = %#v, want suppressed", request)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	var sawCancel bool
	for _, e := range events {
		if e.Type == EventReviewFail {
			t.Fatalf("should not emit review_fail on cancel: %+v", e)
		}
		if e.Type == EventReviewCancel {
			sawCancel = true
		}
	}
	if !sawCancel {
		t.Fatal("expected review_cancel event")
	}
}

func TestProcessQueueRejectsResultAfterSystemSupersedesRequest(t *testing.T) {
	d := openReactorDB(t)
	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "o/r", PRNumber: 3, Title: "t", Author: "a",
		CommitSHA: "sha-old", State: db.PRStateOpen, IsAssigned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := d.CreateReviewRequest(prID, "sha-old")
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	reactor := NewReactor(&config.Config{ReviewTimeout: time.Minute}, d, nil, nil, nil)
	reactor.runReview = func(ctx context.Context, pr db.PullRequest, promptPath string) (*ReviewResult, error) {
		close(started)
		<-release
		return &ReviewResult{
			Review: &db.Review{
				PullRequestID: pr.ID,
				Outcome:       db.ReviewOutcomeApproveWithoutComments,
				Summary:       "late",
			},
		}, nil
	}

	done := make(chan error, 1)
	go func() { done <- reactor.ProcessQueue(context.Background()) }()
	<-started

	if err := d.UpdateReviewRequestStatus(requestID, db.ReviewRequestStatusSuperseded); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("UPDATE pull_requests SET commit_sha = 'sha-new' WHERE id = ?", prID); err != nil {
		t.Fatal(err)
	}
	reactor.CancelSystemRequest(requestID)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	reviews, err := d.ListReviewsForPR(prID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 0 {
		t.Fatalf("superseded request persisted late review: %#v", reviews)
	}
}

func TestProcessQueueFailureStopsAutomaticSameSHARetry(t *testing.T) {
	d := openReactorDB(t)
	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "o/r", PRNumber: 4, Title: "t", Author: "a",
		CommitSHA: "sha-4", State: db.PRStateOpen, IsAssigned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := d.CreateReviewRequest(prID, "sha-4")
	if err != nil {
		t.Fatal(err)
	}

	reactor := NewReactor(&config.Config{ReviewTimeout: time.Minute}, d, nil, nil, nil)
	reactor.runReview = func(context.Context, db.PullRequest, string) (*ReviewResult, error) {
		return nil, errors.New("runner failed")
	}
	if err := reactor.ProcessQueue(context.Background()); err != nil {
		t.Fatal(err)
	}

	request, err := d.GetReviewRequestIncludingTerminal(requestID)
	if err != nil {
		t.Fatal(err)
	}
	if request.Status != db.ReviewRequestStatusFailed {
		t.Fatalf("request status = %q, want failed", request.Status)
	}
	reviews, err := d.ListReviewsForPR(prID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 1 || reviews[0].Outcome != db.ReviewOutcomeToolFailed || reviews[0].CommitSHA != "sha-4" {
		t.Fatalf("failure review = %#v", reviews)
	}
}

func TestCancelRequestMissing(t *testing.T) {
	d := openReactorDB(t)
	r := NewReactor(&config.Config{}, d, nil, nil, nil)
	err := r.CancelRequest(999)
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
