# UI dark IDE redesign

**Date:** 2026-07-30  
**Status:** draft — awaiting user review before implementation plan  
**Scope:** Web UI only (`internal/web/templates/*`, `internal/web/static/style.css`, `internal/web/static/app.js`). Prefer no backend/API/schema changes.

## Problem

The local Code Reviewer console is functional but visually inconsistent and hard to scan: light generic chrome, snake_case outcome strings, missing active nav, mute only on Dashboard, unstyled filter reasons, duplicate “Reviewed externally” copy on History, and uneven Analytics rate badges.

## Goals

- Dark IDE-adjacent look that fits a local ops tool used next to GitHub/devtools.
- Clearer Dashboard and PR-detail hierarchy without changing routes or backend contracts.
- Human-readable status/outcome language everywhere in the UI.
- Fix the audited consistency issues in one redesign pass.

## Non-goals

- Backend, scanner, reactor, or DB changes.
- Embedding PR detail into the Dashboard.
- Replacing Chart.js or SSE.
- Marketing-site visuals or heavy animation.

## Locked decisions

### Visual system (§1 — approved)

Dark IDE tokens (from approved companion mockup):

| Token | Value |
|-------|--------|
| Background | `#0d1117` |
| Surface | `#161b22` |
| Elevated | `#21262d` |
| Border | `#30363d` |
| Text | `#e6edf3` |
| Muted | `#8b949e` |
| Accent / links / focus | `#58a6ff` |
| Open | `#3fb950` |
| Merged | `#a371f7` |
| Success | greens aligned with open/success pills |
| Warning | `#d29922` |
| Danger | `#f85149` |
| External | `#79c0ff` |

Chrome:

- Sticky top nav with **active** link treatment (soft accent fill + inset underline).
- **Mute** control lives in the nav (global), not only on Dashboard.
- Mono for repo paths, short SHAs, and `file:line`.
- Status and outcomes use consistent **pills** (bordered chips), not mixed colored-text vs filled badges.

### Dashboard layout (§2 — approved)

- Single column (not side queue).
- **Pull Requests** list on top (denser rows / cards under the dark system).
- **Review Queue** full width **below** the list.
- Clicking a PR title navigates to `/pr/{id}` (separate page).

### PR detail (§2 — approved)

- Remains route `/pr/{id}` — not inlined on Dashboard.
- Truncated commit SHA (short form in UI; full SHA in `title` tooltip).
- Human-readable outcome labels (never snake_case in visible UI).
- **Summary** = read-only callout.
- **GitHub review comment** = editable textarea (existing general-comment field).
- Publish actions (`Publish full review` / `Publish comments only`) on the **latest** draft only; older drafts muted/inspect-only.
- Section title: prefer **Draft reviews** over “Pending Reviews” when showing stored draft review content.
- Button hierarchy: primary = main publish/request; secondary = comments-only / snippet; danger = delete.

### Secondary pages (§3 — locked with this spec)

Apply the same visual system to History, Filtered, and Analytics.

**History**

- Same card/pill language as Dashboard.
- Do **not** repeat summary text that only restates an outcome badge (e.g. no gray “Reviewed externally” under a `Reviewed externally` pill when that is the whole summary).

**Filtered**

- Filter reasons as muted **filter** pills with friendly labels:

| Stored / internal reason | UI label |
|--------------------------|----------|
| `author` | **author filter** (failed `PR_AUTHORS` allowlist — **not** “opened by me”) |
| `draft` | **draft** (set when PR is a draft; see reconcile) |
| `repo` | **repo filter** (failed `REPOSITORIES` allowlist) |

**Analytics**

- Keep Chart.js stacked trends + period control.
- Outcome table already uses human labels — keep aligned with the shared label map.
- Authors table: apply rate pills consistently to Approval, Human review %, **and** Change request % (same thresholds/styling language as today for approval/human).

## Shared UI label map

Map outcome constants to display strings in templates and Analytics JS (underlying values unchanged):

| Constant | UI label |
|----------|----------|
| `approve_without_comments` | Approve without comments |
| `approve_with_comments` | Approve with comments |
| `changes_requested` | Changes requested |
| `human_review` | Human review |
| `tool_failed` | Tool failed |
| `reviewed_externally` | Reviewed externally |

Queue/request statuses (`pending`, `in progress`, `done`, `failed`) stay short and human (space instead of underscore).

## Audit fixes (must ship with redesign)

1. Active nav state.
2. Mute in global nav.
3. Filter-reason badges styled + friendly labels (esp. **author filter**).
4. Consistent status/outcome pills.
5. No snake_case outcomes in UI.
6. No duplicate “Reviewed externally” text under the badge.
7. Analytics rate-badge consistency (include change-request %).
8. PR detail: truncated SHA; Summary vs editable GitHub comment; publish on latest draft.
9. Prefer templates / CSS / JS only.

## Implementation constraints

- Files in scope: `internal/web/templates/*.html`, `internal/web/static/style.css`, `internal/web/static/app.js` (and existing web tests if assertions depend on copy/HTML).
- Reuse existing routes and APIs.
- Restart via `./run.sh` after UI changes to verify visually.
- Work iteratively: theme/chrome → Dashboard/queue → PR detail → secondary pages; visual verify each step; commit/push per step when doing implementation (later).

## Out of scope for first implementation plan

- New endpoints or DB columns for display labels.
- Redesigning notification toast copy beyond theme colors.
- Mobile-first layout overhaul (keep usable; desktop primary).

## Acceptance criteria

- Dark theme applied across Dashboard, History, Filtered, Analytics, PR detail.
- Nav shows active route; mute available from all pages.
- Outcomes and filter reasons readable; no snake_case in visible UI.
- Dashboard = list then bottom queue; PR detail remains `/pr/{id}`.
- History does not double-print external-review summary when it only mirrors the badge.
- Analytics authors rates use consistent pills including change-request %.
- Existing web unit tests updated if they assert old copy/classes; `go test ./internal/web/...` passes.

## Companion references

Approved/locked mockups under `.superpowers/brainstorm/` (local only):

- Visual system §1
- Layout §2v2 (queue bottom, PR detail separate)
- Secondary pages §3 (labels + Analytics pills)
