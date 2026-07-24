# Implementation Progress

## Phase 1 — Foundation

- [ ] **1. Go module + scaffold**
  - `go mod init`, main.go, package structure
  - dirs: `cmd/`, `internal/config/`, `internal/db/`, `internal/github/`, `internal/review/`, `internal/notify/`, `internal/web/`, `internal/scanner/`
- [ ] **2. Config layer**
  - Env vars via envconfig or viper
  - YAML config file (optional, path via `--config` flag)
  - Precedence: env > yaml > flags
  - Struct: `Config{ GithubToken, Username, PollInterval, ReviewTimeout, ReviewTool, ReviewPromptPath, DBPath, WebHost, WebPort, Filters, Notifications, OwnPRMode, DryRun, LogLevel, ShowThinking, AtlasEnabled }`
- [ ] **3. DB schema + models**
  - SQLite via `modernc.org/sqlite` (pure Go, no CGO)
  - Tables: `pull_requests`, `review_requests`, `reviews`, `review_comments`
  - Soft-delete pattern (created_at, updated_at, deleted_at)
  - Migration on startup (auto-create tables)
- [ ] **4. GitHub client**
  - REST API (simpler than GraphQL for this use case)
  - List PRs assigned to user
  - List PRs by author (own PRs mode)
  - Get PR details, check draft status, check existing reviews
  - Submit PR review (full + inline comments)
  - Rate limit handling → toast notification

## Phase 2 — Core Loops

- [ ] **5. Scanning loop**
  - Periodic ticker (configurable interval)
  - Fetch open PRs from GitHub
  - Reconcile with local DB (new, updated, closed)
  - Track PR by repo + commit SHA combo
  - Skip drafts unless enabled
  - Apply repo/author filters (regex + comma-separated)
  - Log filtered PRs
  - Insert review_requests for new/changed PRs
  - Signal reaction loop
- [ ] **6. Review tool runner**
  - Shell execution of configured tool (agent/codex/claude)
  - Read review prompt from configured path
  - Read output format prompt from embedded Go string
  - Timeout (configurable, default 15 min)
  - Parse JSON output → action + comments
  - Handle failures (parse error, timeout, non-zero exit)
  - Map output to outcomes: approve_without_comments, approve_with_comments, human_review, tool_failed
- [ ] **7. Reaction loop**
  - Periodic ticker + on-demand trigger from scan
  - Process review_requests one by one (sequential queue)
  - Call review tool runner for each
  - Store result in reviews table
  - Emit notifications on start/finish/success/fail
  - Skip if PR was closed/merged meanwhile

## Phase 3 — UI & Notifications

- [ ] **8. Web UI**
  - Go templates + htmx or simple JS for dynamic updates
  - Routes:
    - `/` — dashboard (open PRs, queued reviews)
    - `/pr/{id}` — PR detail (review content, comments, publish)
    - `/analytics` — charts
    - `/filtered` — filtered-out PRs (manual request review)
    - `/api/*` — JSON endpoints for real-time updates
  - SSE for real-time status updates (simpler than websockets)
  - Minimalistic CSS, lightweight design
  - Browser notification permission on first visit
- [ ] **9. Notifications**
  - TTS via macOS `say` command
  - Browser notifications via Notification API
  - Templatable messages with {repo}, {title}, {author}, {pr_number}
  - Events: review_start, review_success, review_fail, human_review_needed

## Phase 4 — Polish

- [ ] **10. Prompts embedding**
  - Embed output_format.md via Go `//go:embed`
  - User-configured review prompt path
- [ ] **11. Analytics page**
  - Time range selector (7d, 30d, quarter, year, all)
  - Group by result, author, repository
  - Bar/line charts (simple SVG or canvas)
- [ ] **12. Wire everything**
  - `main.go` ties all components together
  - Graceful shutdown
  - Startup: both loops begin
  - Error handling: UI errors → toast, backend errors → log

---

## Notes for successor agents

- Config precedence: env var > yaml file > CLI flag. Lowest wins conceptually? Actually env overwrites yaml, yaml overwrites defaults, flags overwrite everything.
- DB schema is auto-migrated on startup — no manual migration tool needed.
- GitHub API: use `google/go-github` v68+ for REST client.
- Review tool is called via `os/exec` — the tool binary must be on PATH.
- Web UI uses SSE for real-time — no websocket dependency.
- Sound files in `sounds/` dir are for streaming via `afplay`, TTS uses `say`.
- The `.env` file is loaded by `run.sh`, NOT by the Go binary itself (keep it 12-factor).
