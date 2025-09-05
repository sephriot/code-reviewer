# Claude Code Project Instructions

This project is an automated GitHub PR code review system. Here are project-specific guidelines for working with this codebase.

## Architecture Overview

- **Asynchronous Design**: The application uses asyncio for non-blocking operations
- **Modular Structure**: Clear separation between GitHub API, Claude integration, monitoring logic, and data persistence
- **Configuration-Driven**: Supports both environment variables and YAML config files
- **Signal Handling**: Graceful shutdown on SIGTERM/SIGINT
- **Smart Tracking**: SQLite database prevents duplicate reviews and tracks history
- **Multi-Modal Actions**: Four different review outcomes including human escalation
- **Cross-Platform Sound**: Audio notifications work on macOS, Linux, and Windows

## Key Components

1. **GitHubMonitor**: Orchestrates the monitoring loop, PR processing, and review decision logic
2. **GitHubClient**: Handles all GitHub API interactions (async) with comprehensive PR data retrieval
3. **ClaudeIntegration**: Manages Claude Code execution, result parsing, and four action types
4. **Config**: Centralized configuration management with validation and path handling
5. **ReviewDatabase**: SQLite-based PR review tracking with smart duplicate prevention
6. **SoundNotifier**: Cross-platform audio notification system for human review alerts

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
- Mock external dependencies (GitHub API, Claude Code CLI)
- Test configuration loading and validation
- Test database operations with temporary SQLite files
- Test sound notification system across platforms

### Security Considerations
- Never log GitHub tokens or sensitive data
- Validate all inputs from GitHub API
- Sanitize file paths when creating temporary files
- Use secure temporary directories for Claude Code execution
- Protect database file with appropriate permissions
- Validate commit SHAs and PR metadata before storage

## Common Tasks

### Adding New Review Actions
1. Extend `ReviewAction` enum in `claude_integration.py`
2. Update `_act_on_review` method in `github_monitor.py`
3. Add corresponding method in `GitHubClient`
4. Update prompt template to support new action

### Customizing GitHub API Interactions
- All API calls should be async using aiohttp
- Handle rate limiting (429 responses)
- Use appropriate GitHub API endpoints
- Validate response data before processing

### Modifying Claude Integration
- Ensure Claude Code CLI is available in PATH
- Use temporary directories for security
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
- Use `should_review_pr()` to check review history before processing
- Call `record_review()` after successful review completion
- Database automatically handles duplicate prevention via unique constraints
- Use `get_review_stats()` for monitoring and analytics

### Sound Notification System
- Cross-platform audio support (macOS, Linux, Windows)
- Graceful fallbacks when audio systems unavailable
- Support for custom sound files via configuration
- Can be completely disabled via configuration

## Dependencies

### Core Dependencies
- `aiohttp`: Async HTTP client for GitHub API
- `click`: Command-line interface
- `pyyaml`: Configuration file parsing
- `python-dotenv`: Environment variable management
- `pygithub`: GitHub API wrapper (used minimally)

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

## Troubleshooting

### Common Issues
- **Rate Limiting**: Increase poll interval, check token scopes
- **Claude Code Errors**: Verify CLI installation and PATH
- **Permission Errors**: Check GitHub token permissions
- **Parsing Errors**: Review prompt template format
- **Database Issues**: Check write permissions for database directory
- **Sound Issues**: Verify audio system availability and file permissions

### Debugging Tips
- Enable DEBUG logging for detailed information
- Use dry run mode to test configuration without GitHub actions
- Check GitHub API response codes and messages
- Verify Claude Code output format matches expectations
- Test with a simple PR first
- Check database content with SQLite tools for review history
- Test sound notifications independently of PR processing

## Future Enhancements

Consider these areas for improvement:
- Webhook-based monitoring instead of polling
- Support for multiple GitHub organizations  
- Custom review rules per repository
- Integration with other AI models
- Web dashboard for monitoring status
- Review analytics and reporting features
- Slack/Teams integration for notifications
- Advanced prompt templating system