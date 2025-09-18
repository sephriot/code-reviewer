# Claude Code Project Instructions

This project is an automated GitHub PR code review system. Here are project-specific guidelines for working with this codebase.

## Architecture Overview

- **Asynchronous Design**: The application uses asyncio for non-blocking operations
- **Modular Structure**: Clear separation between GitHub API, Claude integration, monitoring logic, and data persistence
- **Configuration-Driven**: Supports both environment variables and YAML config files
- **Signal Handling**: Graceful shutdown on SIGTERM/SIGINT
- **Smart Tracking**: SQLite database prevents duplicate reviews using commit SHA comparison and tracks complete history
- **Multi-Modal Actions**: Four different review outcomes including human escalation
- **Cross-Platform Sound**: Audio notifications work on macOS, Linux, and Windows
- **Comprehensive Logging**: Debug and info level logging for all operations with proper shutdown handling
- **Repository Filtering**: Monitor specific repositories or all accessible repositories
- **Enhanced Prompts**: Exemplary review prompt with breaking change detection and clear action guidelines
- **Minimal Data Fetching**: GitHub client only fetches minimal PR info including commit SHAs; the configured LLM CLI handles all detailed data retrieval
- **Web UI Dashboard**: Optional FastAPI-based web interface for managing pending approvals and human reviews
- **Pending Approval Workflow**: `approve_with_comments` actions require human confirmation via web UI before posting to GitHub

## Key Components

1. **GitHubMonitor**: Orchestrates the monitoring loop, PR processing, and review decision logic
2. **GitHubClient**: Handles GitHub API interactions (async) for PR discovery only - returns minimal PR info with commit SHAs
3. **LLMIntegration**: Manages Claude or Codex CLI execution with PR URLs, result parsing, and four action types
4. **Config**: Centralized configuration management with validation and path handling
5. **ReviewDatabase**: SQLite-based PR review tracking with commit SHA-based duplicate prevention and pending approval management with conditional overwrite logic
6. **SoundNotifier**: Cross-platform audio notification system for human review alerts
7. **ReviewWebServer**: FastAPI-based web server providing dashboard for managing pending approvals and human reviews

## Development Guidelines

### Code Style
- Follow PEP 8 conventions
- Use type hints throughout
- Prefer async/await over callbacks
- Use structured logging with appropriate levels

### Error Handling
- Always handle GitHub API rate limits gracefully
- Log errors with context information
- Implement retries for transient failures
- Never expose sensitive information in logs

### Testing Approach
- Unit tests for individual components
- Integration tests for GitHub API interactions
- Mock external dependencies (GitHub API, Claude/Codex CLI)
- Test configuration loading and validation
- Test database operations with temporary SQLite files
- Test sound notification system across platforms

### Security Considerations
- Never log GitHub tokens or sensitive data
- Validate all inputs from GitHub API
- Sanitize file paths when creating temporary files
- Use secure temporary directories for LLM CLI execution
- Protect database file with appropriate permissions
- Validate commit SHAs and PR metadata before storage

## Common Tasks

### Adding New Review Actions
1. Extend `ReviewAction` enum in `llm_integration.py` if model-specific handling is needed
2. Update `_act_on_review` method in `github_monitor.py`
3. Add corresponding method in `GitHubClient`
4. Update prompt template to support new action

### Customizing GitHub API Interactions
- All API calls should be async using aiohttp
- GitHub client only fetches minimal PR identification info (ID, number, repository, URL, head/base commit SHAs)
- Handle rate limiting (429 responses)
- Use appropriate GitHub API endpoints for PR discovery
- Validate response data before processing
- Commit SHAs are essential for tracking review state

### Modifying LLM Integration
- Ensure the selected CLI (Claude or Codex) is available in PATH  
- Pass PR URLs directly to the CLI for data fetching
- The CLI handles all PR information retrieval (files, diffs, metadata)
- Parse output robustly (JSON preferred, text fallback)
- Validate review results before acting

### Configuration Changes
- Update `Config` dataclass with new fields
- Add environment variable mappings
- Update validation logic
- Handle Path objects for file configurations
- Document new options in README

### Database Operations
- All database operations are async and thread-safe
- Use `should_review_pr()` to check review history before processing (compares commit SHAs)
- Call `record_review()` after successful review completion (except for pending approvals)
- Database automatically handles duplicate prevention via unique constraints on (repository, pr_number, head_sha)
- Use `get_review_stats()` for monitoring and analytics
- **Pending Approvals**: Use `create_pending_approval()` for `approve_with_comments` actions with conditional overwrite logic
- **Conditional Overwrites**: Pending approvals are overwritten when new commits arrive, but approved/rejected reviews are preserved
- **Web UI Support**: Methods for retrieving pending approvals, human reviews, and approval history
- **History Tracking**: `get_approved_approvals()` and `get_rejected_approvals()` for complete interaction history
- **Commit-Based Tracking**: Reviews are tied to specific commit SHAs, enabling automatic re-review when PRs are updated

### Sound Notification System
- Cross-platform audio support (macOS, Linux, Windows)
- Graceful fallbacks when audio systems unavailable
- Support for custom sound files via configuration
- Can be completely disabled via configuration
- **Startup notification**: Plays sound when app starts monitoring
- **New PR discovery**: Plays sound for new PRs (dry run mode only)
- **Human review alerts**: Plays sound when complex PRs require human review
- **Pending approval alerts**: Plays sound when `approve_with_comments` creates pending approval

### Web UI Dashboard System
- FastAPI-based REST API with HTML dashboard
- **Pending Approvals Management**: Review and approve/reject `approve_with_comments` actions before GitHub posting
- **Human Review Tracking**: Display PRs marked as `requires_human_review` with reasons and timestamps
- **Approval History**: Complete history of approved and rejected reviews with before/after comparison
- **Real-time Updates**: JavaScript-based interface with async API calls
- **Mobile Responsive**: Works on desktop and mobile devices
- **Configuration Options**: Enable/disable via CLI, environment variables, or config file
- **Security**: Runs on localhost by default, no built-in authentication (single-user design)
- **Database Integration**: New `pending_approvals` table with JSON storage for inline comments and commit SHA tracking

#### Web UI Workflow
1. **Monitor detects PR** → Claude reviews → `approve_with_comments` action
2. **Creates pending approval** → Stores in database → Plays notification sound
3. **Human visits web UI** → Reviews proposed comment and inline feedback
4. **Human approves** → Posts review to GitHub → Records in main reviews table
5. **Human rejects** → Updates status → No GitHub action taken

## Dependencies

### Core Dependencies
- `aiohttp`: Async HTTP client for GitHub API
- `click`: Command-line interface
- `pyyaml`: Configuration file parsing
- `python-dotenv`: Environment variable management
- `pygithub`: GitHub API wrapper (used minimally)
- `fastapi`: Web API framework for dashboard
- `uvicorn`: ASGI server for web interface
- `jinja2`: HTML templating for web UI

### Development Dependencies
- `pytest`: Testing framework
- `pytest-asyncio`: Async test support
- `black`: Code formatting
- `flake8`: Linting
- `mypy`: Type checking

## Deployment Notes

- Application designed to run as a long-running service
- Supports graceful shutdown via signal handlers
- Can be containerized with Docker or run as systemd service
- Database file requires persistent storage in containerized environments
- Monitor logs for GitHub API rate limit warnings
- Dry run mode available for safe testing in production environments
- **Web UI Deployment**: FastAPI runs alongside monitor in same process using asyncio.gather()
- **Port Configuration**: Default web UI on port 8000, configurable via environment variables

## Troubleshooting

### Common Issues
- **Rate Limiting**: Increase poll interval, check token scopes
- **LLM CLI Errors**: Verify CLI installation and PATH
- **Permission Errors**: Check GitHub token permissions
- **Parsing Errors**: Review prompt template format
- **Database Issues**: Check write permissions for database directory
- **Sound Issues**: Verify audio system availability and file permissions
- **Web UI Issues**: Check port availability, verify FastAPI dependencies installed
- **Pending Approvals**: Ensure web UI is enabled when using `approve_with_comments` workflow

### Debugging Tips
- Enable DEBUG logging for detailed information
- Use dry run mode to test configuration without GitHub actions
- Check GitHub API response codes and messages
- Verify model output format matches expectations
- Test with a simple PR first
- Check database content with SQLite tools for review history
- Test sound notifications independently of PR processing
- **GitHub Token**: Use `gh auth token` to get token from GitHub CLI
- **Signal handling**: Test graceful shutdown with SIGTERM/SIGINT
- **Repository filtering**: Verify repository format is `owner/repo`
- **Web UI Testing**: Create sample pending approvals manually or via database queries for testing
- **Pending Approvals**: Check `pending_approvals` table in SQLite for stuck approvals

## Future Enhancements

Consider these areas for improvement:
- Webhook-based monitoring instead of polling
- Support for multiple GitHub organizations  
- Custom review rules per repository
- Integration with other AI models
- ~~Web dashboard for monitoring status~~ ✅ **COMPLETED**
- Review analytics and reporting features
- Slack/Teams integration for notifications
- Advanced prompt templating system
- **Web UI Enhancements**: User authentication, batch operations, review templates
- **Mobile App**: Native mobile interface for approval management
- **API Extensions**: Webhook endpoints for external integrations

## Important Development Notes

### Python Environment
**CRITICAL**: Always use the project's virtual environment when executing Python commands:
```bash
# Correct - use venv
./venv/bin/python -m src.code_reviewer.main
./venv/bin/python tests/test_something.py

# Incorrect - don't use system python
python -m src.code_reviewer.main
python3 tests/test_something.py
```

The venv ensures all dependencies are available and prevents import/version conflicts.

**Model Selection**: Set `REVIEW_MODEL` to `CLAUDE` or `CODEX` (or use the `--model` CLI flag) to choose the review CLI.

### Documentation Maintenance
**IMPORTANT**: This CLAUDE.md file should be updated whenever significant changes are made to the project:
- New features or components
- API changes or new endpoints
- Database schema modifications
- Configuration options
- Workflow changes
- New dependencies

Keep this documentation current to help with future development and troubleshooting.