# Pending Reviews: Single Comment Publish + Snippet Line Numbers

## Problem

On PR detail pending reviews, users can only publish all inline comments at once. Expanding a snippet shows raw code without GitHub-style line numbers.

## Decisions

1. **Single-comment publish** — `POST` publishes one inline comment to GitHub via existing `CreateReviewComment`.
2. **Track published** — `review_comments.published` (0/1). After publish, UI is view-only (no edit/delete/publish); snippet still works. Badge: "Published".
3. **Bulk publish skips published** — "Publish Comments Only" and "Publish Full Review" only include unpublished comments; successfully posted ones are marked published.
4. **Snippet gutter** — `/api/snippet` returns `content`, `start_line`, `target_line`. Expanded UI shows left gutter + highlights the target line.

## Behavior

### Publish one comment

- Confirm dialog → post to GitHub → set `published=1` → reload.
- Failure leaves comment unpublished.
- Published comments: disabled textarea, no Publish/Delete; Snippet remains.

### Bulk publish

- Filter `published=0` before GitHub calls.
- After successful post of a comment, mark it published.
- Full review still marks the review published; only unpublished inlines go in the payload.

### Snippet

- Context window unchanged (±5 lines).
- Gutter shows absolute file line numbers.
- Target line visually highlighted.

## Non-goals

- Editing comments already on GitHub
- Publishing general/summary comment alone
- Diff-side (LEFT/RIGHT) selection beyond existing RIGHT
- Changing snippet context size

## Success criteria

- DB round-trip: mark/list published comments.
- Single publish endpoint posts one comment and marks published.
- Bulk paths omit already-published comments and mark newly posted ones.
- Snippet JSON includes start/target lines; UI shows gutter + highlight.
