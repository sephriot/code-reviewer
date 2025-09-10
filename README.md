# Code Reviewer

An automated GitHub PR code review system using Claude Code. This tool monitors for new pull requests where you're assigned as a reviewer and automatically performs code reviews using Claude AI.

## Features

- 🔍 **Smart PR Monitoring**: Detects new review requests and code changes
- 🤖 **Automated Reviews**: Uses Claude Code for intelligent code analysis
- 📝 **Customizable Prompts**: Tailor review criteria to your needs
- ✅ **Multiple Actions**: Approve, request changes, or flag for human review
- 💬 **Inline Comments**: Specific feedback on problematic code lines
- 🔔 **Sound Notifications**: Audio alerts for PRs requiring human attention
- 🌐 **Web Dashboard**: Optional web interface for managing pending approvals and review history
- 🧠 **Smart Tracking**: Never reviews the same commit twice
- 🗄️ **Review History**: SQLite database tracks all review decisions with complete approval history
- 🏃 **Dry Run Mode**: Test behavior without making actual PR actions
- 🔄 **Continuous Monitoring**: Graceful shutdown with SIGTERM handling

## Prerequisites

- Python 3.8+
- [Claude Code CLI](https://docs.anthropic.com/claude/docs/claude-code) installed and configured
- GitHub Personal Access Token with appropriate permissions
- Git repository access for the repositories you want to monitor

## Installation

1. Clone this repository:
```bash
git clone <repository-url>
cd code-reviewer
```

2. Install the package in development mode:
```bash
pip install -e .
```

Or install dependencies directly:
```bash
pip install -r requirements.txt
```

## Configuration

### Environment Variables

Create a `.env` file in the project root:

```env
GITHUB_TOKEN=your_github_personal_access_token
GITHUB_USERNAME=your_github_username
CLAUDE_PROMPT_FILE=prompts/review_prompt.txt
POLL_INTERVAL=60
LOG_LEVEL=INFO

# Sound notifications
SOUND_ENABLED=true
# SOUND_FILE=sounds/notification.wav

# Dry run mode
DRY_RUN=false

# Database path
DATABASE_PATH=data/reviews.db

# Web UI settings (optional)
WEB_ENABLED=true
WEB_HOST=127.0.0.1
WEB_PORT=8000
```

### GitHub Token Setup

#### Option 1: Use GitHub CLI (Recommended)

If you have the GitHub CLI (`gh`) installed and authenticated:

```bash
# Get your token from GitHub CLI
gh auth token

# Automatically update your .env file
sed -i.bak "s/GITHUB_TOKEN=.*/GITHUB_TOKEN=$(gh auth token)/" .env
```

This uses the same authentication as your `gh` CLI, so no additional setup is needed.

#### Option 2: Create Personal Access Token

If you don't use GitHub CLI, create a Personal Access Token with these scopes:
- `repo` (for private repositories)
- `public_repo` (for public repositories) 
- `pull_requests:read`
- `pull_requests:write`

### Configuration File (Optional)

You can also use a YAML configuration file:

```yaml
# config/config.yaml
github_token: "your_token_here"
github_username: "your_username"
claude_prompt_file: "prompts/custom_prompt.txt"
poll_interval: 30
log_level: "DEBUG"

# Sound notifications
sound_enabled: true
# sound_file: "sounds/notification.wav"

# Dry run and database
dry_run: false
database_path: "data/reviews.db"

# Web UI settings (optional)
web_enabled: true
web_host: "127.0.0.1"
web_port: 8000

# Optional: Specific repositories to monitor (format: owner/repo)
repositories:
  - "owner/repo1"
  - "owner/repo2"
```

## Usage

### Basic Usage

```bash
# Using environment variables
code-reviewer

# Using command line options
code-reviewer --github-token YOUR_TOKEN --github-username YOUR_USERNAME

# Using configuration file
code-reviewer --config config/config.yaml

# Using custom prompt file
code-reviewer --prompt prompts/my_custom_prompt.txt

# Enable web UI dashboard
code-reviewer --web-enabled

# Run with web UI on custom host and port
code-reviewer --web-enabled --web-host 0.0.0.0 --web-port 8080
```

### Command Line Options

- `--config, -c`: Path to configuration file
- `--prompt, -p`: Path to Claude prompt file
- `--github-token`: GitHub personal access token
- `--github-username`: GitHub username to monitor
- `--poll-interval`: Polling interval in seconds (default: 60)
- `--sound-enabled/--no-sound`: Enable/disable sound notifications
- `--sound-file`: Custom sound file for notifications
- `--web-enabled/--no-web`: Enable/disable web UI dashboard
- `--web-host`: Web server host address (default: 127.0.0.1)
- `--web-port`: Web server port (default: 8000)
- `--dry-run`: Log actions instead of performing them

## Customizing Review Prompts

The default prompt is created at `prompts/review_prompt.txt`. You can customize it to match your review requirements:

```text
# Custom Code Review Prompt

You are reviewing a pull request. Analyze the code and respond with JSON:

{
  "action": "approve_with_comment" | "approve_without_comment" | "request_changes" | "requires_human_review",
  "comment": "Comment for approval",
  "summary": "Summary for changes requested",
  "reason": "Why human review is needed",
  "comments": [
    {
      "file": "path/to/file.py",
      "line": 42,
      "message": "Specific feedback"
    }
  ]
}

PR Details:
- Title: {title}
- Author: {author}
- Files: {changed_files}

Focus on:
1. Security vulnerabilities
2. Performance issues  
3. Code style consistency
4. Test coverage
```

## Advanced Usage

### Dry Run Mode

Test the system without making actual GitHub actions:

```bash
# Enable dry run mode
code-reviewer --dry-run

# Combine with other options
code-reviewer --dry-run --no-sound --poll-interval 30
```

### Sound Notifications

Configure audio alerts for PRs requiring human review:

```bash
# Enable sound (default)
code-reviewer --sound-enabled

# Disable sound notifications
code-reviewer --no-sound

# Use custom sound file
code-reviewer --sound-file /path/to/notification.wav
```

### Review Actions

The system can take four different actions based on Claude's analysis:

- **`approve_with_comment`**: Code is good with minor suggestions
- **`approve_without_comment`**: Code is perfect and ready to merge  
- **`request_changes`**: Code has issues that must be fixed
- **`requires_human_review`**: Complex PR needing domain expertise (triggers sound notification)

### Smart Review Tracking

The system automatically tracks review history:

- ✅ **Never reviews the same commit twice**
- 🔄 **Re-reviews when new commits are pushed**
- 🚫 **Permanently skips PRs marked for human review**
- 📊 **Maintains complete audit trail in SQLite database**
- 🌐 **Web dashboard shows complete approval history with before/after comparisons**

### Web Dashboard Features

When web UI is enabled (`--web-enabled` or `WEB_ENABLED=true`):

- 📋 **Pending Approvals**: Review and approve/reject `approve_with_comments` actions before posting to GitHub
- 👤 **Human Reviews**: View all PRs flagged for human attention with reasons and timestamps
- 📚 **Approval History**: Complete history of approved and rejected reviews with:
  - Original vs final comments comparison
  - Original vs final inline comments
  - Original vs final review summaries
  - Direct links to GitHub PRs
- 🔄 **Real-time Updates**: JavaScript interface with automatic refresh
- 📱 **Mobile Responsive**: Works on both desktop and mobile devices

Access the dashboard at `http://localhost:8000` (or your configured host/port).

### Repository Filtering

Limit monitoring to specific repositories:

```bash
# Via environment variable (comma-separated)
REPOSITORIES=owner/repo1,owner/repo2 code-reviewer

# Via config file
repositories:
  - "owner/repo1"
  - "owner/repo2"
```

**Important**: Repository names must be in `owner/repo` format. Invalid formats will be ignored with a warning.

Examples:
- ✅ `microsoft/vscode`
- ✅ `facebook/react`  
- ❌ `vscode` (missing owner)
- ❌ `my-repo` (missing owner)

## Running as a Service

### Using systemd (Linux)

Create a service file at `/etc/systemd/system/code-reviewer.service`:

```ini
[Unit]
Description=Automated Code Reviewer
After=network.target

[Service]
Type=simple
User=your-user
WorkingDirectory=/path/to/code-reviewer
Environment=PATH=/path/to/your/python/bin
ExecStart=/path/to/your/python/bin/code-reviewer
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

Enable and start:
```bash
sudo systemctl enable code-reviewer
sudo systemctl start code-reviewer
```

### Using Docker

#### Build and run manually:

```bash
# Build the image
docker build -t code-reviewer .

# Run with environment file
docker run --env-file .env -v $(pwd)/data:/app/data code-reviewer

# Run with individual environment variables
docker run -e GITHUB_TOKEN=your_token -e GITHUB_USERNAME=your_username code-reviewer
```

#### Using Docker Compose (Recommended):

```bash
# Start the service
docker-compose up -d

# View logs
docker-compose logs -f

# Stop the service
docker-compose down
```

The `docker-compose.yaml` includes:
- Automatic restarts
- Volume mounting for persistent data
- Environment file support
- Health checks

## Development

### Running Tests

```bash
# Install development dependencies
pip install -e .[dev]

# Run tests
pytest tests/

# Run with coverage
pytest --cov=src/code_reviewer tests/
```

### Code Quality

```bash
# Format code
black src/ tests/

# Lint code
flake8 src/ tests/

# Type checking
mypy src/
```

## Architecture

The application consists of several key components:

- **main.py**: Entry point and application orchestration
- **github_monitor.py**: Monitors GitHub for new PRs
- **github_client.py**: GitHub API interactions
- **claude_integration.py**: Claude Code integration
- **config.py**: Configuration management

## Troubleshooting

### Common Issues

1. **"GitHub API rate limit exceeded"**
   - Increase `poll_interval` to reduce API calls
   - Ensure your token has sufficient rate limits

2. **"Claude Code command not found"**
   - Install Claude Code CLI
   - Ensure it's in your PATH

3. **"Permission denied for repository"**
   - Check your GitHub token permissions
   - Verify you have access to the repositories

### Logging

Enable debug logging to troubleshoot issues:

```bash
LOG_LEVEL=DEBUG code-reviewer
```

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests for new functionality
5. Run the test suite
6. Submit a pull request

## License

This project is licensed under the MIT License - see the LICENSE file for details.