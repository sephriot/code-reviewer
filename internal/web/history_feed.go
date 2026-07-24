package web

import (
	"sort"
	"time"

	"github.com/sephriot/code-reviewer/internal/db"
)

// HistoryFeedItem is one PR row on the history feed.
type HistoryFeedItem struct {
	PR         db.PullRequest
	Published  bool
	Outcome    string
	Summary    string
	ActivityAt time.Time
}

// FeedPageMeta describes pagination for the history feed.
type FeedPageMeta struct {
	Page       int
	PageSize   int
	Total      int
	TotalPages int
	HasPrev    bool
	HasNext    bool
	PrevPage   int
	NextPage   int
}

// BuildHistoryFeed merges history PRs with published reviews into one
// deduplicated, activity-sorted feed. prs must already include any
// published-only pull requests the caller wants represented.
func BuildHistoryFeed(prs []db.PullRequest, published []db.PublishedReviewView) []HistoryFeedItem {
	type pubInfo struct {
		outcome   string
		summary   string
		createdAt time.Time
	}
	latest := map[int64]pubInfo{}
	for _, p := range published {
		cur, ok := latest[p.PullRequestID]
		if !ok || p.CreatedAt.After(cur.createdAt) {
			latest[p.PullRequestID] = pubInfo{
				outcome:   p.Outcome,
				summary:   p.Summary,
				createdAt: p.CreatedAt,
			}
		}
	}

	items := make([]HistoryFeedItem, 0, len(prs))
	for _, pr := range prs {
		item := HistoryFeedItem{PR: pr, ActivityAt: pr.UpdatedAt}
		if info, ok := latest[pr.ID]; ok {
			item.Published = true
			item.Outcome = info.outcome
			item.Summary = info.summary
			if info.createdAt.After(item.ActivityAt) {
				item.ActivityAt = info.createdAt
			}
		}
		items = append(items, item)
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].ActivityAt.Equal(items[j].ActivityAt) {
			return items[i].PR.ID > items[j].PR.ID
		}
		return items[i].ActivityAt.After(items[j].ActivityAt)
	})
	return items
}

// PaginateFeed returns a page of items and metadata. page is 1-based;
// values < 1 become 1; values past the end clamp to the last page.
func PaginateFeed(items []HistoryFeedItem, page, pageSize int) ([]HistoryFeedItem, FeedPageMeta) {
	if pageSize <= 0 {
		pageSize = 10
	}
	total := len(items)
	totalPages := 0
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	if page < 1 {
		page = 1
	}
	if totalPages == 0 {
		return nil, FeedPageMeta{Page: 1, PageSize: pageSize, Total: 0, TotalPages: 0}
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > total {
		end = total
	}
	meta := FeedPageMeta{
		Page:       page,
		PageSize:   pageSize,
		Total:      total,
		TotalPages: totalPages,
		HasPrev:    page > 1,
		HasNext:    page < totalPages,
		PrevPage:   page - 1,
		NextPage:   page + 1,
	}
	return items[start:end], meta
}
