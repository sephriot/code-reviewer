# Implementation Progress

> Status: All components implemented (10 commits on `version2`).
> Next: testing, session persistence, review comment edit/delete, chart rendering, own PR review polish.

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

## Notes for successor agents

- Config precedence: env var > yaml file > CLI flag. Lowest wins conceptually? Actually env overwrites yaml, yaml overwrites defaults, flags overwrite everything.
- DB schema is auto-migrated on startup — no manual migration tool needed.
- GitHub API: use `google/go-github` v68+ for REST client.
- Review tool is called via `os/exec` — the tool binary must be on PATH.
- Web UI uses SSE for real-time — no websocket dependency.
- Sound files in `sounds/` dir are for streaming via `afplay`, TTS uses `say`.
- The `.env` file is loaded by `run.sh`, NOT by the Go binary itself (keep it 12-factor).
