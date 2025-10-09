# Agents

## Overview
The code reviewer is an asynchronous service that watches GitHub pull requests and coordinates several focused agents to triage, review, and surface decisions. The orchestration follows the architecture documented in `README.md`, the engineering guidelines in `CLAUDE.md`, and the web workflow described in `WEB_UI_README.md`.

Core behaviors:
- Poll GitHub for review requests using minimal metadata and avoid reprocessing the same commit.
- Delegate code analysis to the configured LLM CLI via structured prompts.
- Store review decisions and pending approvals in SQLite.
- Surface actions through optional web dashboard and sound notifications.

All Python execution should use the project virtual environment (`./venv/bin/python ...`).

## Runtime Flow
1. **Startup**: `src/code_reviewer/main.py` loads configuration (`Config.load`), wires dependencies, handles signals, and (optionally) starts the FastAPI dashboard alongside the monitor (per `README.md`).
2. **Monitoring Loop**: `GitHubMonitor.start_monitoring` (in `src/code_reviewer/github_monitor.py`) polls GitHub on a fixed interval, notifies users, and routes each PR through the review pipeline while respecting dry-run mode and repository/author filters.
3. **Review Execution**: `LLMIntegration.review_pr` (in `src/code_reviewer/llm_integration.py`) runs the configured CLI (Claude or Codex) with the selected prompt, forces JSON-only responses, and converts them into `ReviewResult` instances. Errors propagate as `LLMOutputParseError` for retry handling (see `tests/test_json_failure_handling.py`).
4. **Action Handling**: Based on `ReviewResult.action`, the monitor either posts an approval/change request via `GitHubClient`, inserts a pending approval for human confirmation, or escalates with a human-review flag. All decisions are persisted with commit-SHA awareness (per `ReviewDatabase.should_review_pr`).
5. **Human Oversight**: The optional FastAPI server (`ReviewWebServer` in `src/code_reviewer/web_server.py`) exposes dashboards and REST endpoints for pending approvals, human review queues, and history views, aligning with the workflows laid out in `WEB_UI_README.md`.

## Agent Catalog

### Code Orchestration Agent – `CodeReviewer`
- **Location**: `src/code_reviewer/main.py` (`CodeReviewer` class, `main` CLI entry).
- **Role**: Application bootstrapper; loads configuration, wires clients (`GitHubClient`, `LLMIntegration`, `GitHubMonitor`, optional `ReviewWebServer`), and manages signal-based shutdown (`SIGTERM`/`SIGINT`).
- **Key behaviors**: concurrent execution of monitor and web server via `asyncio.gather`; ensures synchronous cleanup (`monitor.cleanup_sync`) before exit.
- **Configuration inputs**: CLI flags, environment variables, or YAML file as detailed in `README.md` and `CLAUDE.md`.

### Monitoring Agent – `GitHubMonitor`
- **Location**: `src/code_reviewer/github_monitor.py`.
- **Role**: Main event loop that discovers PRs, gates reviews using commit SHA checks, and orchestrates downstream actions.
- **Notable features**:
  - Uses `ReviewDatabase.should_review_pr` to avoid duplicate processing and to respect pending approvals.
  - Differentiates actions: direct approval (`APPROVE_WITHOUT_COMMENT`), human-gated approvals/change requests (stored as pending approvals), and human-review escalations with sound alerts.
  - Automatically reclassifies pending approvals as **OUTDATED** when the PR is merged or closed, keeping the queue focused on actionable work.
  - Supports dry-run logging and fine-grained repository/author filters.
  - Plays startup, notification, and approval sounds through `SoundNotifier`.

### LLM Review Agent – `LLMIntegration`
- **Location**: `src/code_reviewer/llm_integration.py`.
- **Role**: Invokes the selected CLI (Claude or Codex) with contextual prompt templates and enforces JSON output parsing into strongly typed `ReviewResult` objects.
- **Safeguards**:
  - Extracts JSON using pattern matching, validating required fields and action enum values.
   - Raises `LLMOutputParseError` when parsing fails, triggering retry logic in the monitor and logging truncated output for diagnosis (see `CLAUDE.md` guidelines and `tests/test_json_failure_handling.py`).
- **Prompt sourcing**: Uses `Config.prompt_file`, defaulting to `prompts/review_prompt.txt` or user-selected prompt packs (`prompts/adaptive_review_prompt.md`, `...fast...`, `...world_class...`, etc.).
- **Model selection**: Controlled by `Config.review_model` (`REVIEW_MODEL` env or `--model` flag) to switch between CLAUDE and CODEX CLIs.

### GitHub API Agent – `GitHubClient`
- **Location**: `src/code_reviewer/github_client.py`.
- **Role**: Thin async wrapper around GitHub’s REST API for PR discovery and review actions.
- **Capabilities**:
  - Searches for open PRs requesting the configured reviewer and fetches detailed head/base commit SHAs.
  - Posts approvals, change requests, or general comments with optional inline annotations.
  - Manages an aiohttp session lazily and integrates with PyGithub for extended needs.
- **Error handling**: Logs API failures with response payloads; returns boolean success flags so callers can decide on retries/escalations.

### State Management Agent – `ReviewDatabase`
- **Location**: `src/code_reviewer/database.py`.
- **Role**: SQLite-backed persistence for completed reviews, pending approvals, and reporting stats.
- **Highlights**:
  - Thread-safe connections via thread-local storage, auto-migrating schema for edited pending approvals and commit-tracking columns.
  - `should_review_pr` prevents duplicate reviews by checking stored `pr_reviews` and `pending_approvals` against the current head SHA.
  - Provides CRUD helpers for pending approval editing, status transitions, and history queries used by the web UI.
  - Exposes lightweight selectors for pending approval metadata and enforces status transitions (`pending`, `approved`, `rejected`, `outdated`).
  - Exposes analytics such as action counts and per-repository history used in dashboards.

### Human Review Agent – `ReviewWebServer`
- **Location**: `src/code_reviewer/web_server.py`.
- **Role**: FastAPI application serving HTML dashboard and REST APIs for managing pending approvals, human-review queues, and review history.
- **Key workflows** (documented in `WEB_UI_README.md`):
  - Pending approvals tab for editing comments/summaries, approving, or rejecting automated suggestions.
  - Human review tab capturing `REQUIRES_HUMAN_REVIEW` results with direct GitHub links.
  - Approved/Rejected history tabs showing side-by-side comparisons of original vs edited content.
  - Outdated tab surfaces pending approvals that were auto-expired after PR merge/closure.
  - REST endpoints (`/api/pending-approvals`, `/api/outdated-approvals`, `/api/approvals/{id}/approve`, etc.) enabling automation or alternate clients.

### Notification Agent – `SoundNotifier`
- **Location**: `src/code_reviewer/sound_notifier.py`.
- **Role**: Cross-platform async sound playback for new reviews, approvals, and human attention signals.
- **Behavior**:
  - Prefers custom sound files when provided; otherwise falls back to system beeps/output.
  - Distinguishes between general notification and approval sounds, mirroring guidance in `README.md` and `CLAUDE.md`.
  - Gracefully degrades if audio binaries are unavailable, logging warnings instead of raising.

### Configuration Agent – `Config`
- **Location**: `src/code_reviewer/config.py`.
- **Role**: Centralizes configuration loading from YAML, environment variables, and CLI overrides. Ensures prompt file existence (creating a default template when missing) and configures global logging.
- **Relevant docs**: `README.md` (environment variables, CLI flags) and `CLAUDE.md` (development guidance).

### Prompt Packs
- **Location**: `prompts/*.md`.
- **Purpose**: Pre-built prompt strategies (adaptive, exemplary, fast, world-class). Each file encodes different review depth and decision frameworks, as referenced in `README.md` and `CLAUDE.md`. Update `PROMPT_FILE` or CLI `--prompt` flag to switch behavior.

## Data & Decision Flow
```
GitHubMonitor → ReviewDatabase.should_review_pr
    ↳ LLMIntegration.review_pr → ReviewResult
        ↳ (dry run) log only
        ↳ APPROVE_WITHOUT_COMMENT → GitHubClient.approve_pr → ReviewDatabase.record_review
        ↳ APPROVE_WITH_COMMENT / REQUEST_CHANGES → ReviewDatabase.create_pending_approval → (optional) SoundNotifier → ReviewWebServer for human action → GitHubClient + ReviewDatabase.record_review on approval
        ↳ REQUIRES_HUMAN_REVIEW → SoundNotifier + ReviewWebServer human queue
```

## Operational Notes
- Follow the virtual environment rule (`./venv/bin/python`) for all scripting and testing to avoid dependency drift (per `CLAUDE.md`).
- Enable the web UI (`WEB_ENABLED=true` or `--web-enabled`) when working with pending approvals; otherwise, approvals remain in the database awaiting manual processing (`WEB_UI_README.md`).
- Dry run mode (`--dry-run` or `DRY_RUN=true`) exercises the full pipeline without mutating GitHub or the database, useful for prompt tuning and demos.
- Sound notifications can be toggled or customized with env vars (`SOUND_ENABLED`, `SOUND_FILE`, `APPROVAL_SOUND_ENABLED`, `APPROVAL_SOUND_FILE`).

## Extensibility Checklist
- Adding new review actions: update `ReviewAction` enum, `GitHubMonitor._act_on_review`, `GitHubClient`, prompt templates, and any pending approval handling (see “Common Tasks” in `CLAUDE.md`).
- Extending dashboard behavior: modify templates or add API routes in `ReviewWebServer`, coordinate with new database fields/migrations.
- Integrating alternative LLMs: add a new integration agent or extend `LLMIntegration` and adjust `GitHubMonitor` wiring in `main.py`.

Maintaining alignment across `Agents.md`, `README.md`, `CLAUDE.md`, and `WEB_UI_README.md` ensures contributors understand how automated and human-in-the-loop agents interact throughout the review lifecycle.
