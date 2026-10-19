# Code Reviewer

Automated GitHub PR review agent. Scans assigned PRs, runs them through a review tool (Claude/Codex/Agent), stores results, and surfaces them in a web UI.

## Tech Stack

- **Go** — single binary, no runtime deps
- **Go templates** — lightweight server-side rendered UI
- **SQLite** — via `modernc.org/sqlite` (pure Go, no CGO)
- **SSE** — real-time push to browser

## Quick Start

```bash
cp .env.example .env
# edit .env with your GITHUB_TOKEN and GITHUB_USERNAME
go build -o code-reviewer ./cmd/code-reviewer/
./code-reviewer
```

Open http://127.0.0.1:8000

When running in a terminal on macOS or Linux, press **Space** to open the Web UI in your default browser. Press **Ctrl+C** to stop.

## Configuration

Precedence: env vars > YAML config > CLI flags.

See `.env.example` for all options.

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--config` | `CONFIG_PATH` | `config.yaml` | YAML config path |
| `--port` | `WEB_PORT` | `8000` | Web UI port |
| `--host` | `WEB_HOST` | `127.0.0.1` | Web UI host |
| `--log` | `LOG_LEVEL` | `INFO` | Log level |
| `--log-file` | `LOG_FILE` | `data/code-reviewer.log` | Application and review-tool log file; empty disables file logging |

Key env vars: `GITHUB_TOKEN`, `GITHUB_USERNAME`, `REVIEW_TOOL` (CLAUDE/CODEX/AGENT), `REVIEW_AGENT_ARGV` (JSON Agent command override), `REVIEW_TIMEOUT` (seconds), `POLL_INTERVAL` (seconds), `PROMPT_FILE`, `REPOSITORIES`, `PR_AUTHORS`, `PR_AUTHORS_EXCLUSIONS`.

`PR_AUTHORS` is an optional comma-separated list of author usernames or regex patterns to allow. `PR_AUTHORS_EXCLUSIONS` is an optional comma-separated list of usernames or regex patterns to exclude; matching exclusions take precedence over the allow list. Leave it unset or empty to exclude no authors. For example, `PR_AUTHORS_EXCLUSIONS=dependabot\[bot\]` excludes Dependabot.

Agent defaults to `agent --print --output-format json --trust`. To explicitly allow headless tool calls, configure `REVIEW_AGENT_ARGV` with the desired CLI permission flag, for example `--force` or `--yolo`. The default remains approval-protected.

## Architecture

```
┌─────────────┐     ┌──────────────┐     ┌─────────────┐
│  Scanner    │────>│  DB/SQLite   │<────│  Reactor    │
│ (ticker)    │     │              │     │ (ticker)    │
└──────┬──────┘     │ pull_req     │     └──────┬──────┘
       │            │ review_req   │            │
┌──────▼──────┐     │ reviews      │     ┌──────▼──────┐
│  GitHub     │     │ comments     │     │  Runner     │
│  API        │     └──────┬───────┘     │ (os/exec)   │
└─────────────┘            │             └──────┬──────┘
                    ┌──────▼───────┐            │
                    │  Web UI      │     ┌──────▼──────┐
                    │  (SSE+JSON)  │     │  Claude/    │
                    │  Templates   │     │  Codex/Agent│
                    └──────┬───────┘     └─────────────┘
                           │
                    ┌──────▼───────┐
                    │  Notify      │
                    │  (say TTS)   │
                    └──────────────┘
```

Two loops:
1. **Scanning** — polls GitHub for assigned PRs, reconciles with local DB, detects new commits, creates review requests
2. **Reaction** — processes review requests sequentially, invokes review tool, stores results, emits notifications

## Project Structure

```
cmd/code-reviewer/main.go     — entry point, wiring, graceful shutdown
internal/
  config/config.go            — env/yaml/flag loading
  db/db.go                    — SQLite queries, migrations
  db/models.go                — domain types
  github/client.go            — GitHub REST API client
  review/runner.go            — review tool executor, output parser
  review/reactor.go           — review request queue processor
  notify/notifier.go          — TTS (say) + browser notifications
  scanner/scanner.go          — periodic PR scan + reconciliation
  web/server.go               — HTTP server, routes, SSE, API
  web/templates/              — Go HTML templates
  web/static/                 — CSS, JS
```

## Web UI Routes

- `GET /` — Dashboard (open PRs, queue)
- `GET /pr/{id}` — PR detail (review, comments, publish)
- `GET /analytics` — Analytics (period + outcome breakdown)
- `GET /filtered` — Filtered-out PRs (manual request review)
- `GET /events` — SSE stream
- `POST /api/pr/{id}/review` — Request re-review
- `POST /api/review/{id}/publish` — Publish full review to GitHub
- `POST /api/review/{id}/publish-comments` — Publish inline comments only
- `GET /api/analytics?period=30d&group=outcome` — Analytics data

## Review timing analytics

Analytics includes PR-count histograms and daily average/median hours for reviews given and received by `GITHUB_USERNAME`. Timing starts at PR creation and ends at the first submitted review: yours on someone else's PR, or someone else's on yours. Elapsed time includes drafts and weekends. Received reviews exclude bots and self-reviews; repeat reviews and pending reviews do not add samples. Reviews published by this tool under your username count as your reviews.

On startup, a background sync backfills PRs created in the past 365 days, including closed and merged PRs. It covers repositories accessible to `GITHUB_TOKEN`, independently of queue filters, and stores timing separately from the review queue. Completed 30-day ranges are checkpointed so interrupted imports resume after restart. GitHub search ranges are split when they exceed its result limit.

After backfill, the tool refreshes daily while running, looking for PRs updated since the last successful refresh with a one-day overlap. Failed syncs retry hourly. The Analytics page refreshes every minute and shows sync progress, the last successful refresh, and errors; partial or stale results remain visible. Existing timing records are retained, but PRs created more than 365 days ago are not fetched again.

The period filter selects the date of the first review in UTC. PRs without a received review are counted separately by creation date, including closed PRs. The histograms use `<1h`, `1–4h`, `4–12h`, `12–24h`, `1–2d`, `2–7d`, and `≥7d` buckets; exact boundaries enter the next bucket. Daily charts leave gaps when there are no samples. Timing data is also returned under `review_timing` in the analytics API. Rolling back to an older binary leaves the added tables unused.

## Outcomes

| DB value | Meaning |
|----------|---------|
| `approve_without_comments` | Auto-approved, no feedback |
| `approve_with_comments` | Approved with suggestions |
| `human_review` | Escalated — needs human decision |
| `tool_failed` | Review tool crashed/timed out |

## Notifications

- **TTS** via macOS `say` — templatable with `{repo}`, `{title}`, `{author}`, `{pr_number}`
- **Browser** via Notification API — permission requested on first visit
- **Sound files** — `.mp3`/`.wav` played via `afplay`
- Per-event enable/disable + custom template in config
