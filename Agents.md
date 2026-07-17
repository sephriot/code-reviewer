# Agents

## Overview
The code reviewer is an asynchronous service that watches GitHub pull requests and coordinates focused agents to triage, review, and surface decisions. The orchestration follows the architecture documented in `README.md` and the web workflow described in `WEB_UI_README.md`.

Core behaviors:
- Poll GitHub for review requests using minimal metadata and avoid reprocessing the same commit.
- Delegate code analysis to the configured LLM CLI via structured prompts.
- Store review decisions and pending approvals in SQLite.
- Surface actions through optional web dashboard and sound notifications.

All Python execution must use the project virtual environment:
```bash
./venv/bin/python -m src.code_reviewer.main
./venv/bin/python tests/test_something.py
```

Do not use system Python (`python` or `python3`) for project commands.

## Architecture Principles
- **Asynchronous design**: The application uses asyncio for non-blocking operations.
- **Modular structure**: GitHub API access, LLM integration, monitoring, web UI, notifications, and persistence are separated.
- **Configuration-driven runtime**: Environment variables, YAML config files, and CLI flags are supported.
- **Graceful shutdown**: Signal handlers manage SIGTERM/SIGINT cleanup.
- **Smart tracking**: SQLite prevents duplicate reviews using commit SHA comparison and tracks history.
- **Human-in-the-loop actions**: Review outcomes support direct approvals, pending approvals, change requests, and human escalation.
- **Minimal data fetching**: `GitHubClient` fetches only PR identification metadata and commit SHAs; the configured LLM CLI fetches detailed PR data.
- **Optional web dashboard**: FastAPI surfaces pending approvals, human-review queues, and history.
- **Cross-platform notifications**: Sound alerts work on macOS, Linux, and Windows with graceful fallbacks.
- **Comprehensive logging**: Debug and info logs should include enough context for diagnosis without leaking secrets.

## Runtime Flow
1. **Startup**: `src/code_reviewer/main.py` loads configuration (`Config.load`), wires dependencies, handles signals, and optionally starts the FastAPI dashboard alongside the monitor.
2. **Monitoring loop**: `GitHubMonitor.start_monitoring` polls GitHub on a fixed interval, notifies users, and routes each PR through the review pipeline while respecting dry-run mode and repository/author filters.
3. **Review execution**: `LLMIntegration.review_pr` runs the configured CLI (Claude or Codex) with the selected prompt, forces JSON-only responses, and converts them into `ReviewResult` instances. Parsing failures raise `LLMOutputParseError` for retry handling.
4. **Action handling**: Based on `ReviewResult.action`, the monitor posts direct approvals, stores pending human approvals, requests changes, or escalates for human review. Decisions are persisted with commit-SHA awareness.
5. **Human oversight**: `ReviewWebServer` exposes dashboards and REST endpoints for pending approvals, human review queues, and history views.

## Agent Catalog

### Code Orchestration Agent - `CodeReviewer`
- **Location**: `src/code_reviewer/main.py` (`CodeReviewer` class, `main` CLI entry).
- **Role**: Application bootstrapper; loads configuration, wires clients (`GitHubClient`, `LLMIntegration`, `GitHubMonitor`, optional `ReviewWebServer`), and manages shutdown.
- **Key behaviors**: Runs monitor and web server concurrently via `asyncio.gather`; ensures synchronous cleanup (`monitor.cleanup_sync`) before exit.
- **Configuration inputs**: CLI flags, environment variables, or YAML file as detailed in `README.md`.

### Monitoring Agent - `GitHubMonitor`
- **Location**: `src/code_reviewer/github_monitor.py`.
- **Role**: Main event loop that discovers PRs, gates reviews using commit SHA checks, and orchestrates downstream actions.
- **Notable features**:
  - Uses `ReviewDatabase.should_review_pr` to avoid duplicate processing and respect pending approvals.
  - Differentiates direct approval (`APPROVE_WITHOUT_COMMENT`), human-gated approvals/change requests, and human-review escalations.
  - Automatically reclassifies pending approvals as outdated when the PR is merged or closed.
  - Supports dry-run logging and repository/author filters.
  - Plays startup, notification, and approval sounds through `SoundNotifier`.

### LLM Review Agent - `LLMIntegration`
- **Location**: `src/code_reviewer/llm_integration.py`.
- **Role**: Invokes the selected CLI (Claude or Codex) with contextual prompt templates and enforces JSON output parsing into strongly typed `ReviewResult` objects.
- **Safeguards**:
  - Extracts JSON using pattern matching.
  - Validates required fields and action enum values.
  - Raises `LLMOutputParseError` when parsing fails, triggering retry logic in the monitor and logging truncated output for diagnosis.
- **Prompt sourcing**: Uses `Config.prompt_file`, defaulting to `prompts/review_prompt.txt` or user-selected prompt packs.
- **Tool selection**: Controlled by `Config.review_tool` (`REVIEW_TOOL` env or `--tool` flag) to switch between CLAUDE, CODEX, and AGENT CLIs. `REVIEW_MODEL` and `--model` remain deprecated aliases. Claude CLI reviews can also set `Config.claude_model` (`CLAUDE_MODEL`, `--claude-model`) to pass `--model opus|sonnet|fable`.

### GitHub API Agent - `GitHubClient`
- **Location**: `src/code_reviewer/github_client.py`.
- **Role**: Thin async wrapper around GitHub's REST API for PR discovery and review actions.
- **Capabilities**:
  - Searches for open PRs requesting the configured reviewer and fetches detailed head/base commit SHAs.
  - Posts approvals, change requests, or general comments with optional inline annotations.
  - Manages an aiohttp session lazily and integrates with PyGithub for extended needs.
- **Error handling**: Logs API failures with response payloads and returns boolean success flags so callers can decide on retries/escalations.

### State Management Agent - `ReviewDatabase`
- **Location**: `src/code_reviewer/database.py`.
- **Role**: SQLite-backed persistence for completed reviews, pending approvals, and reporting stats.
- **Highlights**:
  - Thread-safe connections via thread-local storage.
  - Auto-migrates schema for edited pending approvals and commit-tracking columns.
  - `should_review_pr` prevents duplicate reviews by checking `pr_reviews` and `pending_approvals` against the current head SHA.
  - `sync_review_requests` atomically replaces the locally cached attention queue after each successful periodic GitHub scan; failed scans preserve the previous snapshot.
  - `create_pending_approval` stores human-gated review results with conditional overwrite logic.
  - Approved/rejected reviews are preserved while new commits can overwrite still-pending approvals.
  - Provides CRUD helpers, history queries, action counts, and per-repository reporting.

### Human Review Agent - `ReviewWebServer`
- **Location**: `src/code_reviewer/web_server.py`.
- **Role**: FastAPI application serving HTML dashboard and REST APIs for managing pending approvals, human-review queues, and review history.
- **Key workflows**:
  - Review Requests tab reads the latest complete review-request snapshot from SQLite without automatic-review repository/author filters and can trigger an on-demand review through `GitHubMonitor.review_pr_on_demand`.
  - Pending approvals tab for editing comments/summaries, approving, or rejecting automated suggestions.
  - Human review tab capturing `REQUIRES_HUMAN_REVIEW` results with direct GitHub links.
  - Unified History tab groups completed, approved, rejected, merged/closed, and expired views.
  - REST endpoints include `/api/review-requests`, `/api/review-requests/review`, `/api/pending-approvals`, and `/api/approvals/{id}/approve`.

### Notification Agent - `SoundNotifier`
- **Location**: `src/code_reviewer/sound_notifier.py`.
- **Role**: Cross-platform async sound playback for new reviews, approvals, and human attention signals.
- **Behavior**:
  - Prefers custom sound files when provided.
  - Falls back gracefully if audio binaries are unavailable.
  - Distinguishes startup, new PR, human review, and pending approval sounds.
- Can be disabled through configuration.
- `SOUND_ENABLED` is the master gate for every sound event; `STARTUP_SOUNDS_ENABLED` controls only startup demo playback.

### Configuration Agent - `Config`
- **Location**: `src/code_reviewer/config.py`.
- **Role**: Centralizes configuration loading from YAML, environment variables, and CLI overrides. Ensures prompt file existence and configures global logging.
- **Relevant docs**: `README.md` for environment variables and CLI flags.

### Prompt Packs
- **Location**: `prompts/*.md`.
- **Purpose**: Pre-built prompt strategies (adaptive, exemplary, fast, world-class). Update `PROMPT_FILE` or the `--prompt` flag to switch behavior.

## Data & Decision Flow
```text
GitHubMonitor -> ReviewDatabase.should_review_pr
    -> LLMIntegration.review_pr -> ReviewResult
        -> (dry run) log only
        -> APPROVE_WITHOUT_COMMENT -> GitHubClient.approve_pr -> ReviewDatabase.record_review
        -> APPROVE_WITH_COMMENT / REQUEST_CHANGES -> ReviewDatabase.create_pending_approval
            -> ReviewWebServer for human action -> GitHubClient + ReviewDatabase.record_review on approval
        -> REQUIRES_HUMAN_REVIEW -> SoundNotifier + ReviewWebServer human queue
```

## Development Guidelines

### Code Style
- Follow PEP 8 conventions.
- Use type hints throughout.
- Prefer async/await over callbacks.
- Use structured logging with appropriate levels.

### Error Handling
- Handle GitHub API rate limits gracefully.
- Log errors with context information.
- Implement retries for transient failures.
- Never expose GitHub tokens or sensitive data in logs.

### Testing Approach
- Unit tests for individual components.
- Integration tests for GitHub API interactions.
- Mock external dependencies such as GitHub API and Claude/Codex CLI.
- Test configuration loading and validation.
- Test database operations with temporary SQLite files.
- Test sound notification behavior across platforms where practical.

### Security Considerations
- Never log GitHub tokens or sensitive data.
- Validate inputs from the GitHub API.
- Sanitize file paths when creating temporary files.
- Use secure temporary directories for LLM CLI execution.
- Protect database files with appropriate permissions.
- Validate commit SHAs and PR metadata before storage.

## Common Tasks

### Adding New Review Actions
1. Extend `ReviewAction` in `llm_integration.py`.
2. Update `GitHubMonitor._act_on_review`.
3. Add the corresponding method in `GitHubClient`.
4. Update prompt templates to support the new action.
5. Update pending approval and web UI handling if the action needs human confirmation.

### Customizing GitHub API Interactions
- Keep API calls async using aiohttp.
- Fetch only minimal PR identification info: ID, number, repository, URL, and head/base commit SHAs.
- Handle rate limiting, including 429 responses.
- Use appropriate GitHub API endpoints for PR discovery.
- Validate response data before processing.
- Preserve commit SHA tracking; it is essential for review state.

### Modifying LLM Integration
- Ensure the selected CLI (Claude or Codex) is available in `PATH`.
- Pass PR URLs directly to the CLI for data fetching.
- Let the CLI handle PR information retrieval: files, diffs, and metadata.
- Prefer JSON output and parse it robustly.
- Validate review results before acting.

### Configuration Changes
- Update the `Config` dataclass with new fields.
- Add environment variable mappings.
- Update validation logic.
- Handle `Path` objects for file configurations.
- Document new options in `README.md`.

### Database Operations
- Keep database operations async and thread-safe.
- Use `should_review_pr()` before processing PRs.
- Call `record_review()` after successful review completion, except for pending approvals.
- Preserve duplicate prevention through unique constraints on `(repository, pr_number, head_sha)`.
- Use `create_pending_approval()` for human-gated review actions.
- Use `get_review_stats()` for monitoring and analytics.
- Keep web UI selectors and history methods aligned with schema changes.

### Sound Notification System
- Support custom sound files via configuration.
- Keep graceful fallbacks when audio systems are unavailable.
- Preserve separate startup, new PR, human review, and pending approval sounds.
- Respect configuration toggles for disabling sound.

### Web UI Dashboard System
- The dashboard is a FastAPI REST API with HTML templates.
- The Review Requests tab reads the latest successful periodic GitHub snapshot from SQLite; repository and author filters only control automatic review selection.
- A successful scan atomically replaces the cached queue, including clearing it when GitHub returns no requests; a failed scan leaves the previous queue intact.
- Review requests can be reviewed on demand through the same assigned-PR pipeline, with immediate start feedback, optional user context, and Claude model override; the endpoint acknowledges from SQLite and revalidates only the selected PR in the background.
- Pending approvals let users review and approve/reject comments before GitHub posting.
- The operational inbox exposes live pending-decision, human-review, and review-request counts; it is navigation only and does not alter cached queue or automatic-review behavior.
- Human review tracking displays PRs marked as `requires_human_review`.
- Closed or merged PRs are archived from active human-review tracking during the poll cleanup; completed entries remain in review history.
- Approval history preserves approved and rejected reviews with before/after comparison.
- History states are grouped under one History tab; analytics remains a separate top-level view.
- The JavaScript interface uses async API calls for updates.
- Tabs support arrow-key, Home, and End navigation; approval/rejection uses a focus-managed modal and status messages are announced through an ARIA live region.
- The UI should remain mobile responsive.
- The server runs on localhost by default and has no built-in authentication; it is designed for a single user.
- The `pending_approvals` table stores inline comments as JSON and tracks commit SHAs.
- The `review_requests` table stores only the current attention snapshot and is not included in review analytics.

#### Web UI Workflow
1. Monitor detects PR.
2. LLM reviews PR and returns `approve_with_comments` or another human-gated action.
3. Monitor creates pending approval, stores it in the database, and plays notification sound.
4. Human reviews proposed comment and inline feedback in the web UI.
5. Approval posts the review to GitHub and records it in the main reviews table.
6. Rejection updates status without taking GitHub action.

## Dependencies

### Core Dependencies
- `aiohttp`: Async HTTP client for GitHub API.
- `click`: Command-line interface.
- `pyyaml`: Configuration file parsing.
- `python-dotenv`: Environment variable management.
- `pygithub`: GitHub API wrapper, used minimally.
- `fastapi`: Web API framework for dashboard.
- `uvicorn`: ASGI server for web interface.
- `jinja2`: HTML templating for web UI.

### Development Dependencies
- `pytest`: Testing framework.
- `pytest-asyncio`: Async test support.
- `black`: Code formatting.
- `flake8`: Linting.
- `mypy`: Type checking.

## Deployment Notes
- The application is designed to run as a long-running service.
- It supports graceful shutdown via signal handlers.
- It can run as a systemd service or directly with the virtual environment.
- The database file requires persistent storage.
- Monitor logs for GitHub API rate limit warnings.
- Dry run mode is available for safe production-like testing.
- The web UI runs alongside the monitor in the same process using `asyncio.gather`.
- Default web UI port is 8000 and is configurable via environment variables.

## Operational Notes
- Enable the web UI (`WEB_ENABLED=true` or `--web-enabled`) when working with pending approvals; otherwise, approvals remain in the database awaiting manual processing.
- Dry run mode (`--dry-run` or `DRY_RUN=true`) exercises the full pipeline without mutating GitHub or the database.
- Sound notifications can be toggled or customized with `SOUND_ENABLED`, `SOUND_FILE`, `APPROVAL_SOUND_ENABLED`, and `APPROVAL_SOUND_FILE`.
- Set `REVIEW_TOOL` to `CLAUDE`, `CODEX`, or `AGENT`, or use the `--tool` CLI flag, to choose the review CLI. `REVIEW_MODEL` and `--model` remain deprecated aliases.
- Set `CLAUDE_MODEL` (or `--claude-model`) to `opus`, `sonnet`, or `fable` to pass Claude CLI `--model` for automated Claude reviews. My PRs one-shot reviews in the web UI can override this per request; leaving the selector on the default uses config.
- Set `REVIEW_EFFORT` (or `--effort`) to tune Claude's reasoning effort (`low`, `medium`, `high`, `xhigh`, `max`). It applies only to the Claude CLI; for other models or invalid values it is logged at startup and ignored (the tool default is used). The effective effort is also logged in the per-PR review startup log.
- Set `OWN_PR_MODE` (or `--own-pr-mode`) to `off`, `auto`, or `manual` to control own PR handling. `auto` reviews own PRs automatically when detected; `manual` tracks them as `pending` in the My PRs tab without reviewing, and a review runs only when explicitly requested via the web UI ("Request Review"). New commits to a manually reviewed PR reset it to `pending`. The legacy `OWN_PR_ENABLED` boolean maps to `auto`/`off` and is ignored when `OWN_PR_MODE` is set. Manual mode requires the web UI to be useful.

## Troubleshooting

### Common Issues
- **Rate limiting**: Increase poll interval and check token scopes.
- **LLM CLI errors**: Verify CLI installation and `PATH`.
- **Permission errors**: Check GitHub token permissions.
- **Parsing errors**: Review prompt template format.
- **Database issues**: Check write permissions for the database directory.
- **Sound issues**: Verify audio system availability and file permissions.
- **Web UI issues**: Check port availability and FastAPI dependencies.
- **Pending approvals**: Ensure the web UI is enabled when using approval workflows.

### Debugging Tips
- Enable DEBUG logging for detailed information.
- Use dry run mode to test configuration without GitHub actions.
- Check GitHub API response codes and messages.
- Verify model output format matches expectations.
- Test with a simple PR first.
- Inspect database content with SQLite tools for review history.
- Test sound notifications independently of PR processing.
- Use `gh auth token` to get a GitHub token from GitHub CLI.
- Test graceful shutdown with SIGTERM/SIGINT.
- Verify repository filters use the `owner/repo` format.
- Create sample pending approvals manually or via database queries when testing the web UI.
- Check the `pending_approvals` table for stuck approvals.

## Future Enhancements
- Webhook-based monitoring instead of polling.
- Support for multiple GitHub organizations.
- Custom review rules per repository.
- Integration with other AI models.
- Review analytics and reporting features.
- Slack/Teams integration for notifications.
- Advanced prompt templating system.
- Web UI authentication, batch operations, and review templates.
- Native mobile interface for approval management.
- Webhook endpoints for external integrations.

## Documentation Maintenance
Update this file whenever significant project changes are made:
- New features or components.
- API changes or new endpoints.
- Database schema modifications.
- Configuration options.
- Workflow changes.
- New dependencies.

Maintain alignment across `Agents.md`, `README.md`, and `WEB_UI_README.md` so contributors understand how automated and human-in-the-loop agents interact throughout the review lifecycle.
