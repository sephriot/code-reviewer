# Analytics Authors Table Implementation Plan

> **For agentic workers:** Use TDD for DB; surgical diffs.

**Goal:** Add a v1-rich Top PR Authors table to `/analytics`, period-aware, via extending `/api/analytics`.

**Architecture:** SQL aggregate join reviews→PRs with comment subquery; rates in Go; attach `authors` on analytics JSON; render table with rate-badge CSS.

**Tech Stack:** Go, SQLite, Go templates.

## Global Constraints

- Extend `/api/analytics` only — no new authors endpoint.
- Soft-delete exclusion for reviews and comments.
- Surgical diffs; TDD for DB layer.

---

### Task 1: DB type + `ReviewsByAuthorStats` (TDD)

**Files:** `internal/db/models.go`, `internal/db/analytics.go`, `internal/db/analytics_test.go`

- [x] Write failing tests: multi-author ranking, rate math, soft-delete skip, since filter, avg comments, limit
- [x] Add `AuthorStats` type
- [x] Implement `ReviewsByAuthorStats(since, limit)`
- [x] `go test ./internal/db/ -run Author -count=1` passes

### Task 2: Wire `apiAnalytics`

**Files:** `internal/web/server.go`

- [x] Call `ReviewsByAuthorStats(since, 15)` when serving outcome analytics
- [x] Set `result["authors"]`; 500 on DB error
- [x] `go build ./cmd/code-reviewer/`

### Task 3: UI table + rate badges

**Files:** `internal/web/templates/analytics.html`, `internal/web/static/style.css`

- [x] Add authors table container; render from `data.authors`
- [x] Rate badge classes matching v1 thresholds
- [x] Rebuild binary; smoke `/analytics`
