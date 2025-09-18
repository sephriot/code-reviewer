"""Data models for the code reviewer application."""

from dataclasses import dataclass
from typing import List, Optional
from enum import Enum


class ReviewModel(Enum):
    """Supported language model CLIs for reviews."""
    CLAUDE = "CLAUDE"
    CODEX = "CODEX"


class ReviewAction(Enum):
    """Possible actions for a PR review."""
    APPROVE_WITH_COMMENT = "approve_with_comment"
    APPROVE_WITHOUT_COMMENT = "approve_without_comment"
    REQUEST_CHANGES = "request_changes"
    REQUIRES_HUMAN_REVIEW = "requires_human_review"


@dataclass
class PRInfo:
    """Information about a GitHub pull request."""
    id: int
    number: int
    repository: List[str]  # [owner, repo]
    url: str
    title: str = ""
    author: str = ""
    head_sha: str = ""
    base_sha: str = ""
    
    @property
    def repository_name(self) -> str:
        """Get the full repository name as 'owner/repo'."""
        return '/'.join(self.repository)
    
    @property
    def owner(self) -> str:
        """Get the repository owner."""
        return self.repository[0]
    
    @property
    def repo(self) -> str:
        """Get the repository name."""
        return self.repository[1]


@dataclass
class InlineComment:
    """An inline comment for a PR review."""
    file: str
    line: int
    message: str


@dataclass
class ReviewResult:
    """Result of a PR code review."""
    action: ReviewAction
    comment: Optional[str] = None
    summary: Optional[str] = None
    reason: Optional[str] = None
    comments: List[InlineComment] = None
    
    def __post_init__(self):
        """Initialize default values after creation."""
        if self.comments is None:
            self.comments = []
    
    @property
    def inline_comments_count(self) -> int:
        """Get the number of inline comments."""
        return len(self.comments)
    
    def to_dict(self) -> dict:
        """Convert to dictionary for JSON serialization."""
        return {
            'action': self.action.value,
            'comment': self.comment,
            'summary': self.summary,
            'reason': self.reason,
            'comments': [
                {
                    'file': comment.file,
                    'line': comment.line,
                    'message': comment.message
                }
                for comment in self.comments
            ]
        }
    
    @classmethod
    def from_dict(cls, data: dict) -> 'ReviewResult':
        """Create ReviewResult from dictionary."""
        comments = []
        for comment_data in data.get('comments', []):
            comments.append(InlineComment(
                file=comment_data['file'],
                line=comment_data['line'],
                message=comment_data['message']
            ))
        
        return cls(
            action=ReviewAction(data['action']),
            comment=data.get('comment'),
            summary=data.get('summary'),
            reason=data.get('reason'),
            comments=comments
        )


@dataclass
class ReviewRecord:
    """Database record of a completed PR review."""
    id: Optional[int]
    repository: str
    pr_number: int
    pr_title: str
    pr_author: str
    review_action: ReviewAction
    review_reason: Optional[str]
    review_comment: Optional[str]
    review_summary: Optional[str]
    inline_comments_count: int
    reviewed_at: str
    pr_updated_at: str
    head_sha: str
    base_sha: str
    created_at: Optional[str] = None
    
    @classmethod
    def from_db_row(cls, row: dict) -> 'ReviewRecord':
        """Create ReviewRecord from database row."""
        return cls(
            id=row['id'],
            repository=row['repository'],
            pr_number=row['pr_number'],
            pr_title=row['pr_title'],
            pr_author=row['pr_author'],
            review_action=ReviewAction(row['review_action']),
            review_reason=row['review_reason'],
            review_comment=row['review_comment'],
            review_summary=row['review_summary'],
            inline_comments_count=row['inline_comments_count'],
            reviewed_at=row['reviewed_at'],
            pr_updated_at=row['pr_updated_at'],
            head_sha=row['head_sha'],
            base_sha=row['base_sha'],
            created_at=row.get('created_at')
        )