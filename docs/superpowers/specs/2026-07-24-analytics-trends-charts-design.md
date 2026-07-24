# Analytics Trends Charts — Design

Date: 2026-07-24  
Status: draft for user review

## Problem

`/analytics` shows period totals and an outcome breakdown table, but no time series. Users want to see how many PRs were reviewed with which outcome over time. PROGRESS.md open item: “Visual analytics charts.”

## Decisions (clarified)

| Topic | Choice |
|-------|--------|
| Scope | Trends-focused (option 1): time series + keep summary cards/table |
| Approach | Extend existing `/api/analytics` + Chart.js CDN (approach A) |
| Chart type | Stacked bar: X = time bucket, stacks = outcomes |
| Bucketing | Day for `7d`/`30d`; week for `quarter`/`year`/`all` |
| UI stack | Go templates + Chart.js CDN; no SPA ([K-000018]) |

## Behavior

1. Period selector unchanged (`7d`, `30d`, `quarter`, `year`, `all`). Default remains `30d`.
2. Summary cards stay: **Reviews Done** (`total`) and **Published** (`published`).
3. Outcome table stays (same `data` map of outcome → count).
4. New chart below cards (above or beside table): stacked bars of reviews per bucket by outcome.
5. Outcomes in stacks (all six): `approve_without_comments`, `approve_with_comments`, `changes_requested`, `human_review`, `tool_failed`, `reviewed_externally`. Colors aligned with existing outcome badge CSS.
6. Missing buckets in the selected window are zero-filled so the X-axis is continuous.
7. Empty state when `total === 0`: message instead of empty chart axes.

## API

`GET /api/analytics?period=…&group=outcome` keeps current fields and adds:

```json
{
  "period": "30d",
  "group": "outcome",
  "since": "...",
  "total": 12,
  "published": 5,
  "data": { "approve_with_comments": 4, "...": 0 },
  "trends": [
    {
      "date": "2026-07-01",
      "total": 3,
      "outcomes": { "approve_with_comments": 2, "human_review": 1 }
    }
  ]
}
```

- Day buckets use key `date` (`YYYY-MM-DD`).
- Week buckets use key `week` (`YYYY-Www` via SQLite `strftime('%Y-%W', …)`), matching v1.
- `outcomes` omits zero outcomes for that bucket; the UI treats missing as 0.
- Soft-deleted reviews excluded (`deleted_at IS NULL`), same as count helpers.

## Data layer

- New DB method, e.g. `ReviewsByOutcomeOverTime(since time.Time, bucket string) ([]TrendBucket, error)`.
- SQL groups by `DATE(created_at)` or `strftime('%Y-%W', created_at)` plus `outcome`.
- Zero-fill buckets in Go (or the handler), not SQL calendar tables.

## Non-goals

- Repo / author charts and tables
- Outcome doughnut chart
- Separate `/api/analytics/trends` (or other v1 multi-endpoint parity)
- Schema migrations
- New JS framework / SPA

## Implementation sketch

- `internal/db`: query + types + table-driven tests with seeded reviews across days/weeks.
- `apiAnalytics`: call trends helper; attach `trends` to JSON; choose day vs week from `period`.
- `analytics.html`: Chart.js CDN script; canvas; render stacked bar from `trends`; destroy/recreate on period change.
- `style.css`: minimal chart layout tweaks only if needed (reuse `.chart-container`).

## Verification

- Unit tests: grouping by day/week, soft-delete exclusion, empty window, multi-outcome same day.
- Manual: open `/analytics`, switch periods, confirm stacks match table totals for the window.
- `go test` for touched packages; `go build ./cmd/code-reviewer/`.

## Atlas

- Relies on [K-000018] Web UI architecture (templates, SSE, no SPA).
- Outcomes from [K-000017] review tool output / outcome mapping.
- Inspired by v1 commit `272dbe1` (Chart.js trends) without full dashboard parity.

## Spec self-review

- [x] No unresolved placeholders
- [x] No contradictions with clarified decisions
- [x] Scope bounded (trends + existing summary/table)
- [x] Success criteria testable via unit tests + page render
