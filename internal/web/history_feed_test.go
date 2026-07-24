package web

import (
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/db"
)

func TestBuildHistoryFeed_DedupAndTags(t *testing.T) {
	t1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	prs := []db.PullRequest{
		{ID: 1, Repo: "o/r", PRNumber: 10, Title: "closed only", Author: "a", State: db.PRStateClosed, UpdatedAt: t1},
		{ID: 2, Repo: "o/r", PRNumber: 11, Title: "closed+published", Author: "b", State: db.PRStateMerged, UpdatedAt: t1},
		{ID: 3, Repo: "o/r", PRNumber: 12, Title: "open reviewed", Author: "c", State: db.PRStateOpen, UpdatedAt: t2},
	}
	published := []db.PublishedReviewView{
		{Review: db.Review{ID: 100, PullRequestID: 2, Outcome: db.ReviewOutcomeApproveWithComments, Summary: "old", Published: true, CreatedAt: t1}, Repo: "o/r", PRNumber: 11, PRTitle: "closed+published", PRAuthor: "b"},
		{Review: db.Review{ID: 101, PullRequestID: 2, Outcome: db.ReviewOutcomeHumanReview, Summary: "latest", Published: true, CreatedAt: t3}, Repo: "o/r", PRNumber: 11, PRTitle: "closed+published", PRAuthor: "b"},
		{Review: db.Review{ID: 102, PullRequestID: 3, Outcome: db.ReviewOutcomeApproveWithoutComments, Summary: "ok", Published: true, CreatedAt: t1}, Repo: "o/r", PRNumber: 12, PRTitle: "open reviewed", PRAuthor: "c"},
	}

	feed := BuildHistoryFeed(prs, published)
	if len(feed) != 3 {
		t.Fatalf("got %d items, want 3", len(feed))
	}

	byID := map[int64]HistoryFeedItem{}
	for _, item := range feed {
		byID[item.PR.ID] = item
	}

	if byID[1].Published {
		t.Error("PR 1 should not be published")
	}
	if !byID[2].Published || byID[2].Outcome != db.ReviewOutcomeHumanReview || byID[2].Summary != "latest" {
		t.Errorf("PR 2 tags/outcome/summary: published=%v outcome=%q summary=%q", byID[2].Published, byID[2].Outcome, byID[2].Summary)
	}
	if !byID[3].Published {
		t.Error("PR 3 should be published")
	}

	// Sort: PR2 activity t3, PR3 activity t2, PR1 activity t1
	if feed[0].PR.ID != 2 || feed[1].PR.ID != 3 || feed[2].PR.ID != 1 {
		t.Errorf("sort order ids: %d, %d, %d", feed[0].PR.ID, feed[1].PR.ID, feed[2].PR.ID)
	}
	if !feed[0].ActivityAt.Equal(t3) {
		t.Errorf("PR2 activity want %v got %v", t3, feed[0].ActivityAt)
	}
}

func TestBuildHistoryFeed_PublishedOnlyPRIncluded(t *testing.T) {
	t1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	prs := []db.PullRequest{
		{ID: 9, Repo: "o/r", PRNumber: 99, Title: "pub only", Author: "z", State: db.PRStateOpen, UpdatedAt: t1},
	}
	published := []db.PublishedReviewView{
		{Review: db.Review{ID: 1, PullRequestID: 9, Outcome: db.ReviewOutcomeHumanReview, Summary: "s", Published: true, CreatedAt: t1}, Repo: "o/r", PRNumber: 99, PRTitle: "pub only", PRAuthor: "z"},
	}
	feed := BuildHistoryFeed(prs, published)
	if len(feed) != 1 || !feed[0].Published {
		t.Fatalf("want one published item, got %+v", feed)
	}
}

func TestPaginateFeed(t *testing.T) {
	items := make([]HistoryFeedItem, 25)
	for i := range items {
		items[i].PR.ID = int64(i + 1)
	}

	page, meta := PaginateFeed(items, 2, 10)
	if len(page) != 10 {
		t.Fatalf("page len %d", len(page))
	}
	if page[0].PR.ID != 11 || page[9].PR.ID != 20 {
		t.Errorf("page 2 range: %d..%d", page[0].PR.ID, page[9].PR.ID)
	}
	if meta.Page != 2 || meta.PageSize != 10 || meta.Total != 25 || meta.TotalPages != 3 {
		t.Errorf("meta %+v", meta)
	}

	page, meta = PaginateFeed(items, 99, 10)
	if meta.Page != 3 || len(page) != 5 {
		t.Errorf("clamp: page=%d len=%d", meta.Page, len(page))
	}

	page, meta = PaginateFeed(nil, 1, 10)
	if meta.Page != 1 || meta.TotalPages != 0 || len(page) != 0 {
		t.Errorf("empty: meta=%+v len=%d", meta, len(page))
	}

	page, meta = PaginateFeed(items, 0, 10)
	if meta.Page != 1 || page[0].PR.ID != 1 {
		t.Errorf("page 0 clamps to 1: meta=%+v first=%d", meta, page[0].PR.ID)
	}
}
