# Web UI for Code Review Management

The code reviewer now includes an optional web UI that provides a dashboard for managing PR reviews, especially for handling `approve_with_comments` actions that require human confirmation.

## Features

### 🎯 **Human Review Dashboard**
- View all PRs marked as `requires_human_review` with reasons
- See timestamp and PR details for each human review requirement
- Direct links to GitHub PRs for easy navigation

### ✅ **Pending Approvals Workflow** 
- **New Behavior**: `approve_with_comments` actions now create pending approvals instead of immediately posting to GitHub
- Review proposed comments and inline feedback before posting
- Edit comments before approval or reject with optional reason
- Complete control over what gets posted to your GitHub PRs

### 📊 **Centralized Management**
- Single dashboard to manage all review activities
- Real-time updates of pending approvals and human reviews
- Clean, responsive interface that works on desktop and mobile

## Configuration

### Environment Variables
```bash
# Enable web UI
WEB_ENABLED=true

# Web server settings (optional)
WEB_HOST=127.0.0.1    # default: 127.0.0.1
WEB_PORT=8000         # default: 8000
```

### Command Line Options
```bash
# Enable web UI
./venv/bin/python -m src.code_reviewer.main --web-enabled

# Customize host and port
./venv/bin/python -m src.code_reviewer.main --web-enabled --web-host 0.0.0.0 --web-port 8080
```

### Configuration File (YAML)
```yaml
web_enabled: true
web_host: "127.0.0.1"
web_port: 8000
```

## Usage Workflow

### 1. Start the Application
```bash
# Start with web UI enabled
./venv/bin/python -m src.code_reviewer.main --web-enabled

# The application will show:
# - GitHub PR monitoring status
# - Web UI URL (e.g., http://127.0.0.1:8000)
```

### 2. Monitor and Review
When the system processes PRs:

- **`approve_with_comments`** → Creates pending approval (requires your review)
- **`approve_without_comment`** → Automatically approves (no web UI interaction)
- **`request_changes`** → Automatically posts changes (no web UI interaction)  
- **`requires_human_review`** → Logged for your attention (visible in web UI)

### 3. Web Dashboard Actions

#### **Pending Approvals Tab**
- **Review & Approve**: Edit proposed comment and post review to GitHub
- **Reject**: Decline the approval with optional reason
- **View PR**: Direct link to GitHub PR

#### **Human Reviews Tab**
- View all PRs that need human attention
- See reason why human review was required
- Direct links to GitHub PRs

## API Endpoints

The web server exposes REST API endpoints:

```bash
# Get pending approvals
GET /api/pending-approvals

# Get PRs requiring human review  
GET /api/human-reviews

# Approve a pending review
POST /api/approvals/{approval_id}/approve
Body: {"comment": "optional modified comment"}

# Reject a pending review
POST /api/approvals/{approval_id}/reject  
Body: {"reason": "optional rejection reason"}

# Get review statistics
GET /api/stats
```

## Database Schema

The web UI adds a new `pending_approvals` table:

```sql
CREATE TABLE pending_approvals (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repository TEXT NOT NULL,
    pr_number INTEGER NOT NULL,
    pr_title TEXT,
    pr_author TEXT,
    pr_url TEXT NOT NULL,
    review_action TEXT NOT NULL,
    review_comment TEXT,
    review_summary TEXT, 
    review_reason TEXT,
    inline_comments TEXT, -- JSON string
    inline_comments_count INTEGER DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    status TEXT DEFAULT 'pending', -- 'pending', 'approved', 'rejected'
    UNIQUE(repository, pr_number)
);
```

## Demo and Testing

### Create Sample Data
```bash
# Generate sample PRs for testing
./venv/bin/python demo_workflow.py
```

This creates:
- 2 PRs requiring human review
- 2 PRs with pending approvals
- Sample data in `demo_reviews.db`

### Test the Web UI
1. Run the demo data script
2. Start the application with web UI enabled
3. Visit the dashboard to see sample data
4. Test approve/reject functionality

## Security Considerations

- Web UI runs on localhost by default (`127.0.0.1`)
- No authentication built-in (intended for single-user local use)
- For production use, consider:
  - Running behind reverse proxy with authentication
  - Using HTTPS
  - Restricting network access

## Architecture Notes

- **FastAPI** framework for REST API and web serving
- **Jinja2** templates for HTML rendering  
- **SQLite** database with new pending approvals table
- **Async/await** throughout for performance
- **Uvicorn** ASGI server integrated with existing asyncio event loop

## Troubleshooting

### Web UI won't start
- Check if port is already in use: `lsof -i :8000`
- Try different port: `--web-port 8080`
- Check virtual environment has all dependencies

### Database errors
- Ensure write permissions in database directory
- Database schema updates automatically on first run
- Delete database file to recreate: `rm data/reviews.db`

### GitHub integration issues
- Verify GitHub token has proper permissions
- Check network connectivity to GitHub API
- Review logs for specific error messages

## Future Enhancements

Potential improvements:
- User authentication/authorization
- Slack/Teams integration for notifications
- Batch approve/reject operations
- Custom filtering and search
- Review templates and saved comments
- Analytics and reporting dashboard