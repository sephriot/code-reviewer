"""Database module for tracking PR review history."""

import asyncio
import json
import logging
import sqlite3
from datetime import datetime, timezone
from pathlib import Path
from typing import Optional, Dict, Any, List
import threading

from .models import PRInfo, ReviewResult, ReviewRecord, ReviewAction


logger = logging.getLogger(__name__)


class ReviewDatabase:
    """SQLite database for tracking PR reviews."""
    
    def __init__(self, db_path: Path):
        self.db_path = db_path
        self._local = threading.local()
        self._ensure_directory()
        self._init_database()
        
    def _ensure_directory(self):
        """Ensure the database directory exists."""
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        
    def _get_connection(self) -> sqlite3.Connection:
        """Get a thread-local database connection."""
        if not hasattr(self._local, 'connection'):
            self._local.connection = sqlite3.connect(
                str(self.db_path),
                isolation_level=None,  # autocommit mode
                check_same_thread=False
            )
            self._local.connection.row_factory = sqlite3.Row
        return self._local.connection
        
    def _init_database(self):
        """Initialize database schema."""
        conn = self._get_connection()
        cursor = conn.cursor()
        
        # Create reviews table
        cursor.execute("""
            CREATE TABLE IF NOT EXISTS pr_reviews (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                repository TEXT NOT NULL,
                pr_number INTEGER NOT NULL,
                pr_title TEXT,
                pr_author TEXT,
                review_action TEXT NOT NULL,
                review_reason TEXT,
                review_comment TEXT,
                review_summary TEXT,
                inline_comments_count INTEGER DEFAULT 0,
                reviewed_at TIMESTAMP NOT NULL,
                pr_updated_at TIMESTAMP,
                head_sha TEXT,
                base_sha TEXT,
                created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
                UNIQUE(repository, pr_number, head_sha)
            )
        """)
        
        # Create pending approvals table
        cursor.execute("""
            CREATE TABLE IF NOT EXISTS pending_approvals (
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
                inline_comments TEXT, -- JSON string of inline comments
                inline_comments_count INTEGER DEFAULT 0,
                created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
                status TEXT DEFAULT 'pending', -- 'pending', 'approved', 'rejected'
                UNIQUE(repository, pr_number)
            )
        """)

        # Create index for faster lookups
        cursor.execute("""
            CREATE INDEX IF NOT EXISTS idx_pr_lookup 
            ON pr_reviews(repository, pr_number)
        """)
        
        cursor.execute("""
            CREATE INDEX IF NOT EXISTS idx_review_action 
            ON pr_reviews(review_action)
        """)
        
        cursor.execute("""
            CREATE INDEX IF NOT EXISTS idx_pending_status 
            ON pending_approvals(status)
        """)
        
        conn.commit()
        logger.info(f"Database initialized at: {self.db_path}")
        
    async def record_review(self, pr_info: PRInfo, review_result: ReviewResult) -> int:
        """Record a completed PR review."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._record_review_sync, pr_info, review_result
        )
        
    def _record_review_sync(self, pr_info: PRInfo, review_result: ReviewResult) -> int:
        """Synchronous implementation of record_review."""
        conn = self._get_connection()
        cursor = conn.cursor()
        
        repository = pr_info.repository_name
        pr_number = pr_info.number
        
        # Count inline comments
        inline_comments_count = review_result.inline_comments_count
        
        try:
            cursor.execute("""
                INSERT OR REPLACE INTO pr_reviews 
                (repository, pr_number, pr_title, pr_author, review_action, 
                 review_reason, review_comment, review_summary, inline_comments_count,
                 reviewed_at, pr_updated_at, head_sha, base_sha)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """, (
                repository,
                pr_number,
                pr_info.title,  # Now using actual PR title
                pr_info.author,  # Now using actual PR author
                review_result.action.value,
                review_result.reason or '',
                review_result.comment or '',
                review_result.summary or '',
                inline_comments_count,
                datetime.now(timezone.utc).isoformat(),
                '',  # PR updated_at - not needed for simplified tracking
                '',  # head_sha - not needed for simplified tracking
                ''   # base_sha - not needed for simplified tracking
            ))
            
            review_id = cursor.lastrowid
            logger.info(f"Recorded review for PR #{pr_number} in {repository} (ID: {review_id})")
            return review_id
            
        except sqlite3.Error as e:
            logger.error(f"Error recording review: {e}")
            raise
            
    async def get_latest_review(self, repository: str, pr_number: int) -> Optional[ReviewRecord]:
        """Get the latest review for a specific PR."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_latest_review_sync, repository, pr_number
        )
        
    def _get_latest_review_sync(self, repository: str, pr_number: int) -> Optional[ReviewRecord]:
        """Synchronous implementation of get_latest_review."""
        conn = self._get_connection()
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT * FROM pr_reviews 
            WHERE repository = ? AND pr_number = ?
            ORDER BY reviewed_at DESC 
            LIMIT 1
        """, (repository, pr_number))
        
        row = cursor.fetchone()
        if row:
            return ReviewRecord.from_db_row(dict(row))
        return None
        
    async def should_review_pr(self, pr_info: PRInfo) -> bool:
        """Determine if a PR should be reviewed based on history - simplified version."""
        repository = pr_info.repository_name
        pr_number = pr_info.number
        
        latest_review = await self.get_latest_review(repository, pr_number)
        
        if not latest_review:
            logger.info(f"PR #{pr_number} in {repository}: No previous review found, will review")
            return True
            
        # For simplified tracking, we'll be more conservative and re-review if it's been a while
        # or if the previous action requires human review (in case human has addressed it)
        action = latest_review.review_action
        logger.info(f"PR #{pr_number} in {repository}: Previous review action was '{action.value}'")
        
        # Don't review again if we recently took a decisive action
        if action in [ReviewAction.APPROVE_WITH_COMMENT, 
                     ReviewAction.APPROVE_WITHOUT_COMMENT,
                     ReviewAction.REQUEST_CHANGES]:
            logger.info(f"PR #{pr_number} in {repository}: Skipping - already processed with decisive action")
            return False
            
        # Always re-review if it was marked for human review (human might have addressed it)
        if action == ReviewAction.REQUIRES_HUMAN_REVIEW:
            logger.info(f"PR #{pr_number} in {repository}: Will review - previous human review requirement may have been addressed")
            return True
            
        return True
        
    async def get_review_stats(self) -> Dict[str, Any]:
        """Get statistics about reviews performed."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_review_stats_sync
        )
        
    def _get_review_stats_sync(self) -> Dict[str, Any]:
        """Synchronous implementation of get_review_stats."""
        conn = self._get_connection()
        cursor = conn.cursor()
        
        # Total reviews
        cursor.execute("SELECT COUNT(*) as total FROM pr_reviews")
        total_reviews = cursor.fetchone()['total']
        
        # Reviews by action
        cursor.execute("""
            SELECT review_action, COUNT(*) as count 
            FROM pr_reviews 
            GROUP BY review_action
        """)
        action_counts = {row['review_action']: row['count'] for row in cursor.fetchall()}
        
        # Recent reviews (last 7 days)
        cursor.execute("""
            SELECT COUNT(*) as recent 
            FROM pr_reviews 
            WHERE reviewed_at > datetime('now', '-7 days')
        """)
        recent_reviews = cursor.fetchone()['recent']
        
        # Unique repositories
        cursor.execute("SELECT COUNT(DISTINCT repository) as repos FROM pr_reviews")
        unique_repos = cursor.fetchone()['repos']
        
        return {
            'total_reviews': total_reviews,
            'reviews_by_action': action_counts,
            'recent_reviews_7d': recent_reviews,
            'unique_repositories': unique_repos
        }
        
    async def get_repository_reviews(self, repository: str, limit: int = 20) -> List[ReviewRecord]:
        """Get recent reviews for a specific repository."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_repository_reviews_sync, repository, limit
        )
        
    def _get_repository_reviews_sync(self, repository: str, limit: int = 20) -> List[ReviewRecord]:
        """Synchronous implementation of get_repository_reviews."""
        conn = self._get_connection()
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT * FROM pr_reviews 
            WHERE repository = ?
            ORDER BY reviewed_at DESC 
            LIMIT ?
        """, (repository, limit))
        
        return [ReviewRecord.from_db_row(dict(row)) for row in cursor.fetchall()]

    async def create_pending_approval(self, pr_info: PRInfo, review_result: ReviewResult) -> int:
        """Create a pending approval record."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._create_pending_approval_sync, pr_info, review_result
        )

    def _create_pending_approval_sync(self, pr_info: PRInfo, review_result: ReviewResult) -> int:
        """Synchronous implementation of create_pending_approval."""
        conn = self._get_connection()
        cursor = conn.cursor()

        # Serialize inline comments to JSON
        inline_comments_json = json.dumps([
            {
                'file': comment.file,
                'line': comment.line,
                'message': comment.message
            }
            for comment in review_result.comments
        ])

        try:
            cursor.execute("""
                INSERT OR REPLACE INTO pending_approvals 
                (repository, pr_number, pr_title, pr_author, pr_url, review_action,
                 review_comment, review_summary, review_reason, inline_comments, 
                 inline_comments_count, status)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """, (
                pr_info.repository_name,
                pr_info.number,
                pr_info.title,
                pr_info.author,
                pr_info.url,
                review_result.action.value,
                review_result.comment or '',
                review_result.summary or '',
                review_result.reason or '',
                inline_comments_json,
                review_result.inline_comments_count,
                'pending'
            ))

            approval_id = cursor.lastrowid
            logger.info(f"Created pending approval for PR #{pr_info.number} in {pr_info.repository_name} (ID: {approval_id})")
            return approval_id

        except sqlite3.Error as e:
            logger.error(f"Error creating pending approval: {e}")
            raise

    async def get_pending_approvals(self, status: str = 'pending') -> List[Dict[str, Any]]:
        """Get pending approvals by status."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_pending_approvals_sync, status
        )

    def _get_pending_approvals_sync(self, status: str = 'pending') -> List[Dict[str, Any]]:
        """Synchronous implementation of get_pending_approvals."""
        conn = self._get_connection()
        cursor = conn.cursor()

        cursor.execute("""
            SELECT * FROM pending_approvals 
            WHERE status = ?
            ORDER BY created_at DESC
        """, (status,))

        approvals = []
        for row in cursor.fetchall():
            approval = dict(row)
            # Parse inline comments JSON
            if approval['inline_comments']:
                approval['inline_comments'] = json.loads(approval['inline_comments'])
            else:
                approval['inline_comments'] = []
            approvals.append(approval)

        return approvals

    async def get_human_review_prs(self) -> List[ReviewRecord]:
        """Get PRs that were marked as requiring human review."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_human_review_prs_sync
        )

    def _get_human_review_prs_sync(self) -> List[ReviewRecord]:
        """Synchronous implementation of get_human_review_prs."""
        conn = self._get_connection()
        cursor = conn.cursor()

        cursor.execute("""
            SELECT * FROM pr_reviews 
            WHERE review_action = ?
            ORDER BY reviewed_at DESC
            LIMIT 50
        """, (ReviewAction.REQUIRES_HUMAN_REVIEW.value,))

        return [ReviewRecord.from_db_row(dict(row)) for row in cursor.fetchall()]

    async def update_pending_approval_status(self, approval_id: int, status: str, user_comment: Optional[str] = None) -> bool:
        """Update the status of a pending approval."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._update_pending_approval_status_sync, approval_id, status, user_comment
        )

    def _update_pending_approval_status_sync(self, approval_id: int, status: str, user_comment: Optional[str] = None) -> bool:
        """Synchronous implementation of update_pending_approval_status."""
        conn = self._get_connection()
        cursor = conn.cursor()

        try:
            update_fields = "status = ?"
            params = [status]
            
            if user_comment is not None:
                update_fields += ", review_comment = ?"
                params.append(user_comment)
            
            params.append(approval_id)
            
            cursor.execute(f"""
                UPDATE pending_approvals 
                SET {update_fields}
                WHERE id = ?
            """, params)

            if cursor.rowcount > 0:
                logger.info(f"Updated pending approval {approval_id} to status: {status}")
                return True
            else:
                logger.warning(f"No pending approval found with ID: {approval_id}")
                return False

        except sqlite3.Error as e:
            logger.error(f"Error updating pending approval: {e}")
            raise

    async def get_pending_approval(self, approval_id: int) -> Optional[Dict[str, Any]]:
        """Get a specific pending approval by ID."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_pending_approval_sync, approval_id
        )

    def _get_pending_approval_sync(self, approval_id: int) -> Optional[Dict[str, Any]]:
        """Synchronous implementation of get_pending_approval."""
        conn = self._get_connection()
        cursor = conn.cursor()

        cursor.execute("""
            SELECT * FROM pending_approvals 
            WHERE id = ?
        """, (approval_id,))

        row = cursor.fetchone()
        if row:
            approval = dict(row)
            # Parse inline comments JSON
            if approval['inline_comments']:
                approval['inline_comments'] = json.loads(approval['inline_comments'])
            else:
                approval['inline_comments'] = []
            return approval
        return None
        
    def close(self):
        """Close database connections."""
        if hasattr(self._local, 'connection'):
            self._local.connection.close()
            delattr(self._local, 'connection')