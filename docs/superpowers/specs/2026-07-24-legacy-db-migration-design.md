# Legacy DB Migration Design (2026-07-24)

## Goal

Import historical review data from `data/reviews.db` (v1) into `data/go-reviewer.db` (v2) for analytics and PR history.

## Sources

| Legacy table | Action |
|---|---|
| `pr_reviews` | Migrate → PRs + done requests + reviews |
| `pending_approvals` | Migrate (skip `pending`) → reviews + inline `review_comments` |
| `own_prs`, `review_started_comments`, queue tables | Skip |
| Pre-existing new-schema tables in `reviews.db` | Ignore (lossy prior import) |

## Mapping

- Actions → outcomes via existing mapActionToOutcome rules
- Dedup key: `(repo, pr_number, commit_sha)` — skip if target already has a review for that SHA
- Existing target PRs: keep live row; only append missing reviews
- New PRs: insert with `needs_review=0`; `state=closed` if legacy status is `merged_or_closed`, else `open`
- `published=1` for `pr_reviews` and `pending_approvals.status=approved`; else `0`
- Prefer `edited_*` fields when non-empty; parse `inline_comments` JSON `{file,line,message}`
- When both sources share a SHA, merge preferring pending text/comments if richer

## Tooling

- Package: `internal/migrate`
- CLI: `cmd/migrate-legacy --from --to [--dry-run]`
- Idempotent: safe to re-run
