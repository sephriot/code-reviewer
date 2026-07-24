# Analytics Per-Author Table — Design

Date: 2026-07-24
Status: approved for implementation

## Problem

`/analytics` shows outcome totals and trends, but not who authors the PRs being reviewed most. v1 had a “Top PR Authors” table with rates (`272dbe1`); it was deferred in the trends work [K-000025].

## Decisions

| Topic | Choice |
|-------|--------|
| Richness | v1-rich: counts + approval / human / changes rates + avg comments |
| API | Extend `GET /api/analytics` with `authors` (same as trends) |
| Period | Respect existing period → `since` |
| Limit | Top 15 authors by review count |
| UI | Table below outcome breakdown; rate badges |

## Behavior

1. Period selector unchanged; authors refresh with period.
2. Columns: Author, PRs Reviewed, Approval Rate, Human Review %, Change Request %, Avg Comments.
3. Approval rate = (`approve_with_comments` + `approve_without_comments`) / total × 100.
4. Soft-deleted reviews and comments excluded.
5. Empty list → “No data” row.

## API

Existing response gains:

```json
{
  "authors": [
    {
      "author": "alice",
      "total_reviews": 10,
      "approval_rate": 70.0,
      "human_review_rate": 20.0,
      "change_request_rate": 10.0,
      "avg_inline_comments": 1.5
    }
  ]
}
```

## Data layer

- `ReviewsByAuthorStats(since time.Time, limit int) ([]AuthorStats, error)`
- Join `reviews` → `pull_requests`; group by `p.author`; filter empty author; order by count DESC; LIMIT.
- Avg comments: average of per-review non-deleted `review_comments` counts.
- Rates computed in Go to 1 decimal place.

## Non-goals

- Repo charts, doughnut, separate `/api/analytics/authors`
- Schema migrations / SPA

## Verification

- DB unit tests: ranking, rates, soft-delete exclusion, period filter, avg comments, limit.
- Manual: `/analytics` shows authors table; period change updates rows.
- `go test ./internal/db/` and `go build ./cmd/code-reviewer/` pass.
