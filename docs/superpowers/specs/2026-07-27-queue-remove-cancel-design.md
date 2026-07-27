# Review Queue Remove + Cancel In-Progress

## Problem

Queued review requests cannot be removed after a mistaken enqueue. In-progress reviews cannot be stopped early.

## Decisions

1. **Soft-delete requests** — Removing a queue item sets `review_requests.deleted_at`. No `reviews` row for pending remove or in-progress interrupt.
2. **Interrupt via context** — Reactor stores a per-active-review `context.CancelFunc`. Cancel kills the tool subprocess through existing `exec.CommandContext`.
3. **Re-queue allowed** — Do not clear `needs_review`; scanner may enqueue again on the next scan.
4. **One API** — `DELETE /api/review-request/{id}` soft-deletes; if that id is the active in-progress review, cancel its context first.
5. **UI** — Dashboard queue rows get a Remove/Stop control that calls the API and drops the row from the DOM.

## Behavior

### Pending / failed

- Soft-delete the request.
- Item disappears from `ListReviewRequests`.
- No history review row.
- No TTS failure notification.

### In progress

- Cancel active context so the subprocess exits.
- Soft-delete the request.
- Reactor treats `context.Canceled` as cancel: no review row, do not set `needs_review=false`, emit a quiet cancel SSE event (optional toast, no failure TTS).
- Continue processing any remaining pending items.

### Errors

- Unknown / already deleted / already `done` → 404.
- Soft-delete is idempotent enough that a late cancel after delete is harmless.

## Non-goals

- `cancelled` review outcome / history rows
- Preventing scanner re-queue
- Cancel confirmation dialogs beyond a simple button
- Fixing Review Queue `#0` display (tracked separately as Atlas K-000030)

## Success criteria

- Soft-deleted requests no longer appear in the queue list.
- Pending remove leaves no review row and leaves `needs_review` intact.
- Canceling in-progress stops the tool run, leaves no review row, leaves `needs_review` intact.
- Dashboard can remove/stop a queue item without a full page reload.
