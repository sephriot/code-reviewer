# Re-review with Additional Input — Pending Approvals & Human Review tabs

**Date:** 2026-06-03
**Status:** Approved

## Problem

When triggering "Review Again" for a pending approval (or a human-review record) in the
web dashboard, the user cannot provide additional input/guidance for the fresh LLM review.

The capability already exists end-to-end for the **Own PRs** tab: its "Review Again"
button reveals an inline textarea and submits `{ user_context }` to the API. The backend
endpoints for the other two tabs already accept the same field:

- `POST /api/approvals/{approval_id}/review-again` parses `user_context` from the JSON
  body (`web_server.py:417-423`) and forwards it to `LLMIntegration.review_pr`.
- `POST /api/reviews/{review_id}/review-again` does the same (`web_server.py:554-560`).
- `review_pr` injects the context into the prompt: "Please take this additional context
  into account when performing your review…" (`llm_integration.py:194`).

The gap is frontend-only: `reviewAgain()` and `reviewHumanReviewAgain()` in
`src/code_reviewer/templates/dashboard.html` (lines 701-743) show a `confirm()` dialog
and POST with **no body**, so user input is never collected.

## Success Criteria

1. On the **Pending Approvals** tab, clicking "Review Again" reveals an inline context
   area (textarea + Submit Review / Cancel) instead of a `confirm()` dialog.
2. On the **Human Review** tab, the same pattern applies.
3. Submitting posts `{ user_context: <text> }` with `Content-Type: application/json` to
   the existing endpoint; empty input is allowed (backend treats blank as no context).
4. Cancel hides the area and clears the textarea; no request is sent.
5. UX matches the existing Own PRs pattern (same placeholder text, same button styles).
6. No backend changes.

## Design

Replicate the Own PRs inline-toggle pattern (`dashboard.html:1452-1498`) per tab:

### Pending Approvals card (`dashboard.html:362`)
- Add a hidden `<div id="approval-context-${approval.id}" ...>` with
  `<textarea id="approval-context-input-${approval.id}">` (placeholder: "Optional:
  provide context for the re-review (e.g. what changed, what to focus on)...") and
  Submit Review / Cancel buttons.
- "Review Again" button calls `toggleApprovalContext(id)` to reveal the area.
- New `submitApprovalReview(id)` reads the textarea and POSTs
  `{ user_context }` to `/api/approvals/${id}/review-again`; success/error handling is
  identical to the current `reviewAgain` (toast + `refreshCurrentView()`).
- `cancelApprovalContext(id)` hides and clears.
- The old `confirm()` is dropped — the explicit Submit/Cancel step replaces it,
  matching the Own PRs tab which has no confirm.

### Human Review card (`dashboard.html:393`)
- Identical treatment: `toggleHumanReviewContext(id)` / `submitHumanReviewAgain(id)` /
  `cancelHumanReviewContext(id)` posting to `/api/reviews/${id}/review-again`.

### Out of scope
- Refactoring the three tabs to share one component (would touch working Own PRs code).
- Backend changes, schema changes, prompt changes.

## Error handling

Unchanged: non-OK responses surface via `alert(...)` with response text (including the
busy/409 message from the single-flight review lock); network errors are logged and
alerted, as today.

## Testing

- The change is template-only JavaScript; verify manually in the dashboard:
  toggle/cancel behavior, submit with and without context, toast on success.
- End-to-end signal: the backend logs `"Starting re-review ... with user context"`
  (`web_server.py:476`, `web_server.py:605`) only when a non-empty `user_context`
  arrives — confirm it fires on submit-with-text and not on submit-empty.
- Known pre-existing gap (not addressed here): `user_context` prompt injection in
  `LLMIntegration._run_model_cli` (`llm_integration.py:192-197`) has no automated
  coverage — `tests/test_review_prompt_context.py` covers only `previous_pending`
  context, and all existing tests override `_run_model_cli`, bypassing the real
  prompt assembly. This gap also applies to the existing Own PRs flow.
