# Code reviewer — design (as built)

Retrospective product design for a single-user, local GitHub PR review agent.
Use this document as the guideline if you rebuild a similar tool. It reflects
what shipped in the Go v2 app and the decisions ironed out after the original
brief — not an aspirational wishlist.

---

## Purpose

Automate first-pass review of GitHub pull requests assigned to one human:

1. Discover open PRs that still need that user's review.
2. Run an external CLI review tool (`claude` / `codex` / `agent`) against each.
3. Store structured outcomes and inline comments locally.
4. Let the human inspect, edit, and selectively publish to GitHub.
5. Notify on review lifecycle events (TTS + browser).

The human remains the publisher. The tool never auto-publishes reviews.

## Constraints

| Constraint | Choice |
|---|---|
| Users | Exactly one operator on one machine |
| OS | macOS (TTS via `say`, sounds via `afplay`) |
| Auth | GitHub token from environment / config |
| Persistence | Local SQLite; keep history forever (soft-delete only) |
| Rate limits | Surface a toast / log; no elaborate backoff product |
| Process model | Single binary; two background loops + local web UI |

## Non-goals

Multi-user auth, webhooks, CI integration, Slack/email, multi-tenant hosting,
hard data pruning, and any SPA framework. Prefer the simplest thing that works.

---

## Architecture

Three cooperating pieces share one SQLite database:

```
┌─────────────┐     ┌──────────────┐     ┌─────────────┐
│  Scanner    │────>│  SQLite      │<────│  Reactor    │
│  (ticker)   │     │              │     │  (ticker)   │
└──────┬──────┘     │ pull_req     │     └──────┬──────┘
       │            │ review_req   │            │
┌──────▼──────┐     │ reviews      │     ┌──────▼──────┐
│  GitHub API │     │ comments     │     │  Runner     │
└─────────────┘     └──────┬───────┘     │  (os/exec)  │
                           │             └──────┬──────┘
                    ┌──────▼───────┐            │
                    │  Web UI      │     ┌──────▼──────┐
                    │  SSE + REST  │     │ Claude/     │
                    │  templates   │     │ Codex/Agent │
                    └──────┬───────┘     └─────────────┘
                           │
                    ┌──────▼───────┐
                    │  Notifier    │
                    │  say/afplay  │
                    └──────────────┘
```

Suggested package boundaries (keep them sharp):

| Package | Role |
|---|---|
| `config` | Load defaults → YAML → env → flags; validate |
| `db` | Schema, queries, soft-delete, analytics |
| `github` | REST client (assigned PRs, own PRs, review submit, file content) |
| `scanner` | Poll GitHub, reconcile rows, enqueue review requests |
| `review` | Runner (exec + parse) and reactor (sequential queue) |
| `notify` | TTS / sound / browser notification payloads |
| `web` | Templates, SSE, REST API |
| `migrate` (optional) | One-shot import from a legacy DB |

Startup: open DB (auto-migrate), reset orphaned `in_progress` queue rows to
`pending`, start scanner ticker, start reactor ticker, start web server.
Both loops run for the life of the process.

---

## Data model

### Identity (critical)

A pull request row is uniquely identified by **`(repo, pr_number)`**, not by
commit SHA.

- `commit_sha` is the **change detector**: when it changes, mark prior work
  outdated and enqueue a new review.
- Reviews store the SHA they were produced against (`reviews.commit_sha`).

Do not key the PR table on SHA — the same PR accumulates history across
commits.

### Tables

All entities carry `created_at`, `updated_at`, `deleted_at` (soft-delete).
Queries ignore soft-deleted rows. No automatic pruning.

**`pull_requests`**

| Field | Meaning |
|---|---|
| `repo`, `pr_number` | Unique identity (`owner/repo` + number) |
| `title`, `author`, `commit_sha`, `draft` | Cached GitHub metadata |
| `state` | `open` \| `closed` \| `merged` |
| `needs_review` | Still awaiting this user's attention |
| `is_outdated` | Superseded by a newer commit / review cycle |
| `filtered_reason` | Why it was kept off the main queue (`draft`, repo/author filter, …); empty when eligible |

**`review_requests`** — persistent queue

| Field | Meaning |
|---|---|
| `pull_request_id` | Target PR |
| `status` | `pending` \| `in_progress` \| `done` \| `failed` |

**`reviews`** — one result per completed (or failed) run

| Field | Meaning |
|---|---|
| `outcome` | See Outcomes |
| `commit_sha` | SHA reviewed |
| `summary`, `general_comment` | Tool prose |
| `published` | Whether a full GitHub review was submitted for this row |

**`review_comments`** — inline comments belonging to a review

| Field | Meaning |
|---|---|
| `file`, `line`, `message` | GitHub review-comment shape |
| `published` | Per-comment publish flag (bulk publish skips already-published) |

Schema strategy that worked: `CREATE TABLE IF NOT EXISTS` plus idempotent
`ALTER TABLE … ADD COLUMN` on open. No versioned migration framework needed
at this scale. SQLite stores timestamps as TEXT (`datetime('now')`); the DB
layer must scan them into `time.Time` explicitly.

### Outcomes

Stored outcome strings:

| Outcome | Meaning |
|---|---|
| `approve_without_comments` | Ship as-is |
| `approve_with_comments` | Approve with suggestions |
| `changes_requested` | Blocking issues |
| `human_review` | Escalate to the operator |
| `tool_failed` | Exec/timeout/parse failure |
| `reviewed_externally` | Operator (or scanner) recorded that GitHub already has their review |

Tool JSON uses slightly different action names; map them deliberately:

| Tool `action` | DB outcome |
|---|---|
| `approve_without_comment` | `approve_without_comments` |
| `approve_with_comment` | `approve_with_comments` |
| `request_changes` | `changes_requested` |
| `requires_human_review` | `human_review` |
| anything else / parse failure | `tool_failed` |

Keep `request_changes` as its own outcome — do not fold it into
`approve_with_comments`. Publishing maps outcomes to GitHub review events
(`APPROVE`, `REQUEST_CHANGES`, `COMMENT` as appropriate).

---

## Scanning loop

Periodic ticker (default 60s). On each tick:

1. Fetch open PRs assigned to the configured username.
2. Optionally fetch the user's own open PRs (`own_pr_mode`: `off` \| `manual` \| `auto`; default `off`).
3. Deduplicate by `(owner, repo, number)`.
4. For each candidate, fetch details and reconcile:

| Situation | Behaviour |
|---|---|
| Draft | Store with `filtered_reason=draft`; do not auto-enqueue (drafts skipped by default) |
| Repo / author filter miss | Store with a filter reason; log the skip; do not auto-enqueue |
| Closed / merged | Set `state` accordingly; history tab owns them — not the filtered list |
| User already reviewed on GitHub | Clear `needs_review`; may record `reviewed_externally` |
| New PR or SHA changed | Upsert; set `needs_review`; create a `pending` review request |
| Same SHA, still needs review | Do **not** blindly re-enqueue; re-check GitHub review status |
| Same SHA, already reviewed locally | Keep on dashboard as reviewed; no new queue item |

Filters: config lists of regex / plain patterns for repositories (`owner/repo`,
`owner/.*`) and authors. Log every discard with the matching reason.

**Merged vs closed:** GitHub's PR state API is only `open`/`closed`. Use the
merged flag (`GetMerged` / equivalent) and store `state=merged` so history can
distinguish them. Existing `closed` rows update on the next detail reconcile.

After enqueueing new work, signal the reactor (or let its ticker notice).

---

## Reaction loop (queue)

Sequential processor over `review_requests` where `status=pending` and not
soft-deleted. **One review at a time.**

For each item:

1. Mark `in_progress` (DB guard against concurrent workers / double ticks).
2. Cancel previous in-flight context if any; bind a new context with the review timeout (default 15 minutes).
3. Invoke the runner.
4. Persist a `reviews` row (+ `review_comments`); mark request `done` or `failed`.
5. Emit a domain event for notifications and SSE.

### Queue UX that proved necessary

- **Remove pending:** soft-delete the `review_requests` row.
- **Cancel in-progress:** cancel the per-request context so `CommandContext`
  kills the child process. Do **not** write a review row for a cancel; leave
  `needs_review=true` so the scanner can re-queue later.
- **Startup recovery:** any row left `in_progress` after a crash → reset to
  `pending`.

---

## Review runner

`os/exec` of the configured tool. The operator supplies a prompt file path.
The app **embeds** an output-format contract and appends it to every run so
the tool is forced into machine-readable JSON.

### Tool output contract (embed this)

```json
{
  "action": "approve_with_comment" | "approve_without_comment" | "request_changes" | "requires_human_review",
  "comment": "...",
  "summary": "...",
  "reason": "...",
  "comments": [{ "file": "path", "line": 42, "message": "..." }]
}
```

Parsing rules that matter in practice:

- Strip non-JSON prefix/suffix (find first `{` and last `}`).
- Prefer the tool's final result event when using stream-json.
- Log full stream to a file; keep terminal output to start/complete/fail lines.
- Propagate context deadline into `CommandContext`.

### Tool invocation

Support at least: `CLAUDE`, `CODEX`, `AGENT`.

- Default Agent argv should stay approval-protected (`--print`, JSON output,
  `--trust`). Permission bypass (`--force` / `--yolo`) only via explicit
  config override — never as a silent default.
- When “show thinking” is on, use stream-json and avoid flags known to hang
  before the terminal result.

Pass enough PR context in the prompt (repo, number, title, author, SHA, URL)
so the tool can fetch the diff itself via its own GitHub access.

---

## Publishing (human-gated)

From the PR detail UI the operator can:

1. **Publish full review** — GitHub PR Review with outcome event + general
   comment + unpublished inline comments.
2. **Publish comments only** — inline comments without a review verdict.
3. **Publish one inline comment** — single comment; set `review_comments.published`.

Bulk publish must skip already-published comments and mark newly posted ones.
Published comments become view-only in the UI.

Also support: request another review round, edit comment text before publish,
delete unpublished comments, confirm destructive actions in a modal.

Code snippets for inline comments: fetch on demand from GitHub file content
(API returns content + start line); do not store blobs in SQLite. Render with
line gutters and highlight the target line. Link `file:line` to GitHub source.

---

## Notifications

Two flavours, independently configurable:

1. **TTS / sound** — `say:` templates or audio file paths played with `afplay`.
2. **Browser** — Notification API; request permission on first visit. Clicking
   a notification should open the app's PR detail page.

Placeholder syntax: simple `{repo}`, `{title}`, `{author}`, `{number}` (or
`{pr_number}`) replacement — not a full template language.

Emit distinct events at least for:

- review started
- approved (with/without comments as needed)
- changes requested
- human review needed
- tool failed / timeout
- merged or closed (optional)
- own-PR ready / needs attention (when own-PR mode is on)

Per-event enable flags + custom templates in config.

---

## Web UI

Server-rendered Go templates, light CSS, no SPA framework. Real-time via
**SSE** (`GET /events`) — not websockets. Pages refresh selected state from
events without full reloads.

### Pages

| Path | Purpose |
|---|---|
| `/` | Dashboard: open PRs + review queue (remove / cancel) |
| `/pr/{id}` | Detail: reviews, inline comments, publish / edit / re-review |
| `/analytics` | Period selector, outcome breakdown, trends chart, top authors |
| `/history` | Merged activity feed (closed/merged ∪ published), paginated |
| `/filtered` | Filtered-out opens with manual “request review” |

### API surface (minimum)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/pr/{id}/review` | Enqueue re-review |
| `DELETE` | `/api/review-request/{id}` | Dequeue or cancel |
| `POST` | `/api/review/{id}/publish` | Full review to GitHub |
| `POST` | `/api/review/{id}/publish-comments` | Inline comments only |
| `POST` | `/api/inline-comment/{id}/publish` | Single comment |
| `GET` | `/api/snippet` | On-demand file snippet |
| `GET` | `/api/analytics` | Period + grouping + authors + trends |

UI error from an operator action → toast. Backend loop error → log only.

### History feed

One row per PR: union of history-eligible PRs and PRs with published reviews.
Annotate with state badge + optional `published` + latest outcome. Sort by
`max(pr.updated_at, latest_published.created_at)`. In-memory pagination is
fine at single-user SQLite scale.

### Analytics

Dimensions: time, repository, author, outcome.

Default period: last 30 days. Also: week, month, quarter, year, all time.

Ship at least:

- summary cards (reviews done vs published)
- outcome breakdown table
- stacked bar trends over time (day buckets for short periods, week for long)
- top authors table with approval / human / changes rates and avg comments

---

## Configuration

Precedence (highest wins): **environment variables → YAML file → CLI flags /
defaults** applied as: load defaults, overlay YAML, overlay env, overlay flags.

YAML path via `--config` / `CONFIG_PATH`. Annotate every env var in an
`.env.example`. The binary should not secretly load `.env`; a launcher script
may.

Essential knobs:

- `GITHUB_TOKEN`, `GITHUB_USERNAME`
- `REVIEW_TOOL`, `PROMPT_FILE`, `REVIEW_TIMEOUT`, `REVIEW_AGENT_ARGV`
- `POLL_INTERVAL`, `DATABASE_PATH`, `LOG_LEVEL`, `LOG_FILE`
- `REPOSITORIES`, `PR_AUTHORS`, `OWN_PR_MODE`
- `WEB_HOST`, `WEB_PORT`, `WEB_ENABLED`
- Sound master toggle + per-event enables/templates

---

## Telemetry

Unstructured logs to stdout (and optional append-only log file). Log scan
decisions (especially filters), queue transitions, tool start/PID/duration,
publish results. Keep interactive terminal noise low when stream logging is
on — detail belongs in the file.

---

## Error handling

| Source | Behaviour |
|---|---|
| UI / API action | JSON error + toast |
| Scanner / reactor | Log; continue next tick |
| GitHub rate limit | Visible notification; retry next poll |
| Tool timeout / crash | `tool_failed` review row + failure notification |

---

## Implementation lessons worth keeping

1. **PR identity ≠ commit identity.** Upsert by `(repo, number)`; SHA drives
   re-review.
2. **Embed the output contract** in the runner. External prompt files alone
   will drift.
3. **Persistent queue + single worker + startup reset** beats in-memory jobs
   for a long-lived local daemon.
4. **Cancel must kill the child** (`CommandContext`); soft-delete alone is not
   enough for in-progress work.
5. **SSE is enough** for a single-user dashboard; skip websocket complexity.
6. **Human publish gate** with per-comment published flags prevents duplicate
   GitHub noise.
7. **Store filtered PRs** (with reason) so the UI can offer manual review —
   skipping before insert makes `/filtered` useless.
8. **Normalize merged vs closed** at ingest time for honest history.
9. **Agent permission bypass is opt-in.** Defaults must not `--force`.
10. **Soft-delete everywhere** if you promise “keep history, operator deletes.”

---

## Open gaps (still true relative to the ideal)

Track these if cloning the design — they were intended but are incomplete or
uneven in the reference implementation:

- Filtered-out PRs must reliably persist with `filtered_reason` for `/filtered`
  + manual enqueue (route exists; ingestion must not drop them).
- Own-PR mode: scanner can list them; reactor/notifications should treat
  `manual` vs `auto` distinctly end-to-end.
- Comment edit/delete API parity with UI (DB helpers ready; wire fully).
- Pass prior review context into re-reviews so the tool does not repeat itself.
- Graceful shutdown: wait for in-flight scanner/reactor work on signal.

---

## Suggested build order

1. Config + SQLite schema/models  
2. GitHub client (list assigned, details, has-reviewed, submit)  
3. Scanner reconcile + enqueue  
4. Runner + embedded output contract  
5. Reactor + events  
6. Web dashboard + PR detail + SSE  
7. Notifications  
8. Publish paths + snippets  
9. Analytics + history + filtered  
10. Queue cancel/remove + legacy migrate (if needed)

Keep each package independently testable with table-driven Go tests around
reconcile rules, outcome mapping, and feed helpers — those rules are where
silent product bugs hide.
