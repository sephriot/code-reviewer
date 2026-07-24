# History Merged Feed — Design

Date: 2026-07-24  
Status: draft for user review

## Problem

`/history` shows two unpaginated sections (Closed PRs, then Published Reviews). Users want one chronological feed with clear annotations and pagination.

## Decisions (clarified)

| Topic | Choice |
|-------|--------|
| List shape | Single merged list (not two sections) |
| Dedup | One row per PR; both tags when applicable |
| Sort | `max(PR.updated_at, latest published review.created_at)` |
| Approach | In-memory merge + page slice (10/page) |
| UI stack | Keep Go templates; no new JS framework ([K-000018]) |

## Behavior

1. Membership: PR appears if it is in the existing history set (`ListHistoryPRs`) **or** it has at least one published review.
2. Each row shows:
   - Repo, PR number, title, author (same card pattern as today)
   - State badge from PR (`open` / `closed` / `merged` as stored)
   - `published` badge when any published review exists
   - When published: latest review outcome badge; short summary snippet (latest by `created_at`)
   - Link to `/pr/{id}`
3. Sort descending by activity time = `max(PR.updated_at, latestPublished.CreatedAt)` (if no published review, use `PR.updated_at` only).
4. Pagination: 10 rows per page via `?page=N` (default 1). Prev/Next and page indicator below the list. Out-of-range pages clamp to last page (or page 1 if empty).
5. Empty state: single message when the merged feed has zero PRs.

## Non-goals

- SQL-level `LIMIT`/`OFFSET` feed query
- Client-side merge of two full lists
- Changing dashboard, filtered, analytics, or publish flows
- Editing review content from history

## Implementation sketch

- `historyPage`: load `ListHistoryPRs` + `ListPublishedReviews`, merge by `pull_request_id`, sort, paginate, pass feed + page meta to template.
- Prefer a small pure helper (e.g. `internal/web` or `internal/db`) for merge/sort so it is unit-testable.
- Replace two sections in `history.html` with one ranged list + pagination controls.
- Keep existing list DB methods; no schema change.

## Verification

- Unit tests for merge: tags, sort key, dedup, empty published, published-only PR, page boundaries (page size 10).
- Manual or handler-level check: `/history` and `/history?page=2` render expected counts and badges.

## Atlas

- Relies on [K-000018] Web UI architecture (templates, no SPA).

## Spec self-review

- [x] No unresolved placeholders
- [x] No contradictions with clarified decisions
- [x] Scope bounded (history tab only)
- [x] Success criteria testable via unit tests + page render
