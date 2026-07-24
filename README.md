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

## Configuration

Precedence: env vars > YAML config > CLI flags.

See `.env.example` for all options.

| Flag | Env | Default | Description |
|------|-----|---------|-------------|
| `--config` | `CONFIG_PATH` | `config.yaml` | YAML config path |
| `--port` | `WEB_PORT` | `8000` | Web UI port |
| `--host` | `WEB_HOST` | `127.0.0.1` | Web UI host |
| `--log` | `LOG_LEVEL` | `INFO` | Log level |

Key env vars: `GITHUB_TOKEN`, `GITHUB_USERNAME`, `REVIEW_TOOL` (CLAUDE/CODEX/AGENT), `REVIEW_TIMEOUT` (seconds), `POLL_INTERVAL` (seconds), `PROMPT_FILE`, `REPOSITORIES`, `PR_AUTHORS`.

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
