# Pending Comment Publish + Snippet Line Numbers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Allow publishing one inline comment with persistent published state, and show GitHub-style line numbers (with target highlight) in expanded snippets.

**Architecture:** Add `published` on `review_comments`; DB helpers Get/Publish; web handler posts via `CreateReviewComment`. Bulk publish filters unpublished and marks posted. Snippet API returns start/target lines; PR detail JS renders gutter.

**Tech Stack:** Go, SQLite, Go templates, vanilla JS/CSS.

## Global Constraints

- Surgical diffs only for this feature.
- TDD for DB layer.
- Follow existing `ALTER TABLE ... ADD COLUMN` migrate pattern.

---

### Task 1: DB published flag + helpers

**Files:**
- Modify: `internal/db/models.go`
- Modify: `internal/db/db.go` (migrate, ListReviewComments scan, PublishReviewComment, GetReviewComment)
- Test: `internal/db/comments_test.go`

- [x] Write failing tests for PublishReviewComment / List includes Published
- [x] Implement migrate ALTER + model field + Get/Publish + update List SELECT
- [x] `go test ./internal/db/ -count=1`

### Task 2: Single publish API + bulk skip/mark

**Files:**
- Modify: `internal/web/server.go`

- [x] Extend `apiInlineComment` for `.../publish` POST
- [x] Filter unpublished in bulk; mark after successful GH post
- [x] Build compiles

### Task 3: Snippet start/target lines

**Files:**
- Modify: `internal/github/client.go` and/or `apiSnippet`
- Modify: `internal/web/server.go`

- [x] Return start_line + target_line from `/api/snippet`

### Task 4: UI

**Files:**
- Modify: `internal/web/templates/pr_detail.html`
- Modify: `internal/web/static/style.css`

- [x] Publish button + published badge / view-only
- [x] Snippet gutter + highlight rendering
- [x] Rebuild and smoke
