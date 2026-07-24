# Implementation Progress

> Status: All components implemented (11 commits on `version2`).
> Build: `go build ./cmd/code-reviewer/ && go vet ./...` clean.

## ✅ Phase 1 — Foundation

- [x] **1. Go module + scaffold** (`bc7bcbc`)
- [x] **2. Config layer** (`2e3900b`)
- [x] **3. DB schema + models** (`ef2fd85`)
- [x] **4. GitHub client** (`4947885`)

## ✅ Phase 2 — Core Loops

- [x] **5. Scanning loop** (`7497676`)
- [x] **6. Review tool runner** (`fa0494b`)
- [x] **7. Reaction loop** (`50e67bf`)

## ✅ Phase 3 — UI & Notifications

- [x] **8. Web UI** (`56c7853`)
- [x] **9. Notifications** (`5ef8b56`)

## ✅ Phase 4 — Polish

- [x] **10. Prompts embedding** (output format embedded in `internal/review/runner.go`)
- [x] **11. Analytics page** (table, period selector, outcome breakdown)
- [x] **12. Wire everything** (`f9878e1`)

---

## 📋 Open Items (not blockers, needs attention)

- [ ] **Review comment edit/delete API** — `server.go` has a stub DELETE handler returning 501. Need edit endpoint too. `db.go` has `UpdateReviewComment`/`DeleteReviewComment` ready.
- [ ] **Own PR mode review flow** — Scanner fetches own PRs but the reactor needs to handle them distinctly (e.g. different outcome treatment, notification).
- [ ] **Session / context passing across reviews** — When retrying review of same PR, the tool should receive previous review context so it doesn't repeat itself. No mechanism yet.
- [ ] **Visual analytics charts** — Table view works. SVG/Canvas chart rendering not implemented.
- [ ] **Filtered PRs visibility** — Scanner skips filtered PRs before storing. Need to store them in DB with a `filtered_out` flag so the `/filtered` page can show them.
- [ ] **Test suite** — No tests yet. Go table-driven tests for each package.
- [ ] **Review prompt context builder** — `Runner.BuildReviewPromptContext` exists but is never called. The actual PR diff/Cli context needs to be passed to the review tool.
- [ ] **Graceful shutdown in tickers** — `main.go` uses `for/select` with `ctx.Done()` in the main goroutine, but the scanner/reactor goroutines might still be mid-flight. Need a WaitGroup or similar.

## Notes for successor agents

- Config precedence: env var > yaml file > CLI flag. Lowest wins conceptually? Actually env overwrites yaml, yaml overwrites defaults, flags overwrite everything.
- DB schema is auto-migrated on startup — no manual migration tool needed.
- GitHub API: use `google/go-github` v68+ for REST client.
- Review tool is called via `os/exec` — the tool binary must be on PATH.
- Web UI uses SSE for real-time — no websocket dependency.
- Sound files in `sounds/` dir are for streaming via `afplay`, TTS uses `say`.
- The `.env` file is loaded by `run.sh`, NOT by the Go binary itself (keep it 12-factor).
