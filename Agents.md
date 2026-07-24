# AGENTS.md — Code Reviewer (Go)

Go rewrite of automated GitHub PR review agent. Polls assigned PRs, runs review tool, stores results, serves Web UI.

## Architecture

Two loops + Web UI:

1. **Scanner** — periodic ticker. Fetches assigned PRs from GitHub, reconciles with SQLite, detects new commits (SHA change), creates review requests. Applies repo/author regex filters. Skips drafts. Checks if user already reviewed.

2. **Reactor** — processes review request queue sequentially. Invokes runner, stores result, emits events. One PR at a time.

3. **Runner** — `os/exec` of Claude/Codex/Agent CLI. Reads prompt file from config. Has embedded output format prompt. Parses JSON output. 15min timeout.

4. **Web UI** — Go templates, SSE for real-time push, REST API. Dashboard / PR detail / Analytics / Filtered pages.

5. **Notifier** — macOS `say` TTS + browser Notification API. Templatable messages per event.

## Key files

| File | Purpose |
|------|---------|
| `cmd/code-reviewer/main.go` | Entry point, wiring, tickers |
| `internal/config/config.go` | Env / YAML / flag loading |
| `internal/db/db.go` | SQLite queries, auto-migration |
| `internal/db/models.go` | Domain types + outcome constants |
| `internal/github/client.go` | GitHub REST API via go-github |
| `internal/review/runner.go` | Tool exec + JSON output parse |
| `internal/review/reactor.go` | Queue processing + events |
| `internal/notify/notifier.go` | TTS say + browser notifications |
| `internal/scanner/scanner.go` | PR scan + reconciliation |
| `internal/web/server.go` | HTTP server, SSE, API, templates |

## DB schema

- `pull_requests` — repo + number unique, tracks commit_sha, needs_review, is_outdated, state, soft-delete
- `review_requests` — queue by PR, status pending/in_progress/done/failed
- `reviews` — outcome (approve_without_comments/approve_with_comments/human_review/tool_failed), summary, published
- `review_comments` — file, line, message per review

All tables have created_at / updated_at / deleted_at.

## Commands

```bash
go build ./cmd/code-reviewer/     # build binary
./code-reviewer                    # run (needs .env sourced)
./code-reviewer --port 8888       # override port
./code-reviewer --config prod.yaml # use YAML config
```

## Config precedence

Env var > YAML file > CLI flag. YAML path via `--config` flag. See `.env.example` for all 50+ settings.

## Dependencies

- `modernc.org/sqlite` — pure Go SQLite
- `google/go-github/v68` — GitHub REST client
- `golang.org/x/oauth2` — token auth
- `gopkg.in/yaml.v3` — YAML parsing

## Open items

See `PROGRESS.md` → Open Items section.

## Atlas

Project: `global/code-reviewer`
Atoms tracked: architecture decisions, progress notes, gotchas.
