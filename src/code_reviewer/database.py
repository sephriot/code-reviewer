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


PENDING_APPROVAL_STATUS_PENDING = 'pending'
PENDING_APPROVAL_STATUS_APPROVED = 'approved'
PENDING_APPROVAL_STATUS_REJECTED = 'rejected'
PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED = 'merged_or_closed'
PENDING_APPROVAL_STATUS_EXPIRED = 'expired'

VALID_PENDING_APPROVAL_STATUSES = {
    PENDING_APPROVAL_STATUS_PENDING,
    PENDING_APPROVAL_STATUS_APPROVED,
    PENDING_APPROVAL_STATUS_REJECTED,
    PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED,
    PENDING_APPROVAL_STATUS_EXPIRED,
}


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
                -- Edited versions (null = no edits made)
                edited_review_comment TEXT,
                edited_review_summary TEXT, 
                edited_inline_comments TEXT, -- JSON string of edited inline comments
                head_sha TEXT NOT NULL,
                base_sha TEXT,
                created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
                status TEXT DEFAULT 'pending', -- 'pending', 'approved', 'rejected', 'merged_or_closed', 'expired'
                UNIQUE(repository, pr_number, head_sha)
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
        
        # Add new columns for edited content if they don't exist (migration)
        try:
            cursor.execute("ALTER TABLE pending_approvals ADD COLUMN edited_review_comment TEXT")
        except sqlite3.OperationalError:
            pass  # Column already exists
            
        try:
            cursor.execute("ALTER TABLE pending_approvals ADD COLUMN edited_review_summary TEXT")
        except sqlite3.OperationalError:
            pass  # Column already exists
            
        try:
            cursor.execute("ALTER TABLE pending_approvals ADD COLUMN edited_inline_comments TEXT")
        except sqlite3.OperationalError:
            pass  # Column already exists

        # Add head_sha and base_sha columns for commit-based pending approval tracking
        try:
            cursor.execute("ALTER TABLE pending_approvals ADD COLUMN head_sha TEXT NOT NULL DEFAULT ''")
        except sqlite3.OperationalError:
            pass  # Column already exists
            
        try:
            cursor.execute("ALTER TABLE pending_approvals ADD COLUMN base_sha TEXT")
        except sqlite3.OperationalError:
            pass  # Column already exists

        # Rename legacy statuses for clarity
        try:
            cursor.execute(
                "UPDATE pending_approvals SET status = ? WHERE status = 'outdated'",
                (PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED,)
            )
        except sqlite3.OperationalError:
            pass

        # Migrate UNIQUE constraint to include head_sha (if needed)
        self._migrate_pending_approvals_unique_constraint(cursor)

        conn.commit()
        logger.info(f"Database initialized at: {self.db_path}")

    def _migrate_pending_approvals_unique_constraint(self, cursor: sqlite3.Cursor):
        """Migrate pending_approvals table to include head_sha in UNIQUE constraint.

        This migration is safe because:
        1. Old constraint: UNIQUE(repository, pr_number) - max 1 row per PR
        2. New constraint: UNIQUE(repository, pr_number, head_sha) - max 1 row per PR per commit
        3. Since old constraint was MORE restrictive, all existing data is already valid for new constraint
        4. We copy ALL data to preserve everything
        """
        try:
            # Check if the table exists
            cursor.execute("""
                SELECT sql FROM sqlite_master
                WHERE type='table' AND name='pending_approvals'
            """)
            row = cursor.fetchone()

            if not row:
                # Table doesn't exist yet, will be created with correct constraint
                logger.debug("pending_approvals table doesn't exist yet, will be created with correct schema")
                return

            schema_sql = row[0] if row else ""

            # Check if the UNIQUE constraint already includes head_sha
            if "UNIQUE(repository, pr_number, head_sha)" in schema_sql:
                logger.debug("pending_approvals UNIQUE constraint already includes head_sha - no migration needed")
                return

            # Count existing rows before migration
            cursor.execute("SELECT COUNT(*) as count FROM pending_approvals")
            row_count_before = cursor.fetchone()[0]
            logger.info(f"Starting pending_approvals migration - {row_count_before} rows to migrate")

            # Create new table with correct schema
            cursor.execute("""
                CREATE TABLE pending_approvals_new (
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
                    inline_comments TEXT,
                    inline_comments_count INTEGER DEFAULT 0,
                    edited_review_comment TEXT,
                    edited_review_summary TEXT,
                    edited_inline_comments TEXT,
                    head_sha TEXT NOT NULL,
                    base_sha TEXT,
                    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
                    status TEXT DEFAULT 'pending',
                    UNIQUE(repository, pr_number, head_sha)
                )
            """)

            # Check for rows with NULL or empty head_sha
            cursor.execute("""
                SELECT COUNT(*) as count FROM pending_approvals
                WHERE head_sha IS NULL OR head_sha = ''
            """)
            invalid_sha_count = cursor.fetchone()[0]

            if invalid_sha_count > 0:
                logger.warning(
                    f"Found {invalid_sha_count} pending approval(s) with missing head_sha - "
                    f"these will be marked as expired since they're invalid"
                )

            # Copy data from old table, handling NULL/empty head_sha
            # Rows with invalid head_sha are marked as 'expired' and given placeholder SHA
            cursor.execute("""
                INSERT INTO pending_approvals_new
                SELECT
                    id,
                    repository,
                    pr_number,
                    pr_title,
                    pr_author,
                    pr_url,
                    review_action,
                    review_comment,
                    review_summary,
                    review_reason,
                    inline_comments,
                    inline_comments_count,
                    edited_review_comment,
                    edited_review_summary,
                    edited_inline_comments,
                    COALESCE(NULLIF(head_sha, ''), 'migration-placeholder-' || id) as head_sha,
                    base_sha,
                    created_at,
                    CASE
                        WHEN head_sha IS NULL OR head_sha = '' THEN 'expired'
                        ELSE status
                    END as status
                FROM pending_approvals
                ORDER BY id
            """)

            # Verify no data loss
            cursor.execute("SELECT COUNT(*) as count FROM pending_approvals_new")
            row_count_after = cursor.fetchone()[0]

            if row_count_before != row_count_after:
                raise sqlite3.Error(
                    f"Data loss detected during migration! Before: {row_count_before}, After: {row_count_after}"
                )

            if invalid_sha_count > 0:
                logger.info(
                    f"Migrated {invalid_sha_count} row(s) with invalid head_sha "
                    f"(marked as expired with placeholder SHA)"
                )

            logger.info(f"Successfully copied all {row_count_after} rows to new table")

            # Drop old table
            cursor.execute("DROP TABLE pending_approvals")

            # Rename new table
            cursor.execute("ALTER TABLE pending_approvals_new RENAME TO pending_approvals")

            # Recreate index
            cursor.execute("""
                CREATE INDEX IF NOT EXISTS idx_pending_status
                ON pending_approvals(status)
            """)

            logger.info("✅ Successfully migrated pending_approvals table with ZERO data loss")

        except sqlite3.Error as e:
            logger.error(f"❌ Error during pending_approvals migration: {e}")
            # Try to rollback by restoring old table if new table was created
            try:
                cursor.execute("""
                    SELECT name FROM sqlite_master
                    WHERE type='table' AND name='pending_approvals_new'
                """)
                if cursor.fetchone():
                    cursor.execute("DROP TABLE pending_approvals_new")
                    logger.info("Rolled back migration - dropped temporary table")
            except:
                pass
            raise  # Re-raise to prevent app from starting with broken schema

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
                pr_info.title,
                pr_info.author,
                review_result.action.value,
                review_result.reason or '',
                review_result.comment or '',
                review_result.summary or '',
                inline_comments_count,
                datetime.now(timezone.utc).isoformat(),
                '',  # PR updated_at - not needed for commit-based tracking
                pr_info.head_sha,  # Track the reviewed commit SHA
                pr_info.base_sha   # Track the base commit SHA
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

    async def get_review_for_commit(self, repository: str, pr_number: int, head_sha: str) -> Optional[ReviewRecord]:
        """Get the review for a specific commit of a PR."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_review_for_commit_sync, repository, pr_number, head_sha
        )
        
    def _get_review_for_commit_sync(self, repository: str, pr_number: int, head_sha: str) -> Optional[ReviewRecord]:
        """Synchronous implementation of get_review_for_commit."""
        conn = self._get_connection()
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT * FROM pr_reviews 
            WHERE repository = ? AND pr_number = ? AND head_sha = ?
            ORDER BY reviewed_at DESC
            LIMIT 1
        """, (repository, pr_number, head_sha))
        
        row = cursor.fetchone()
        if row:
            return ReviewRecord.from_db_row(dict(row))
        return None

    async def delete_review_for_re_review(self, repository: str, pr_number: int, head_sha: str) -> bool:
        """Delete review record to allow re-review of the same commit."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._delete_review_for_re_review_sync, repository, pr_number, head_sha
        )

    def _delete_review_for_re_review_sync(self, repository: str, pr_number: int, head_sha: str) -> bool:
        """Synchronous implementation of delete_review_for_re_review."""
        conn = self._get_connection()
        cursor = conn.cursor()

        try:
            cursor.execute("""
                DELETE FROM pr_reviews
                WHERE repository = ? AND pr_number = ? AND head_sha = ?
            """, (repository, pr_number, head_sha))

            deleted = cursor.rowcount > 0
            if deleted:
                logger.info(
                    f"Deleted review record for {repository}#{pr_number} at {head_sha[:8]} for re-review"
                )
            return deleted

        except sqlite3.Error as e:
            logger.error(f"Error deleting review for re-review: {e}")
            raise

    async def get_pending_approval_for_commit(self, repository: str, pr_number: int, head_sha: str) -> Optional[dict]:
        """Get existing pending approval for a specific commit of a PR."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_pending_approval_for_commit_sync, repository, pr_number, head_sha
        )
        
    def _get_pending_approval_for_commit_sync(self, repository: str, pr_number: int, head_sha: str) -> Optional[dict]:
        """Synchronous implementation of get_pending_approval_for_commit."""
        conn = self._get_connection()
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT * FROM pending_approvals 
            WHERE repository = ? AND pr_number = ? AND head_sha = ? AND status = ?
            ORDER BY created_at DESC
            LIMIT 1
        """, (repository, pr_number, head_sha, PENDING_APPROVAL_STATUS_PENDING))
        
        row = cursor.fetchone()
        if row:
            return dict(row)
        return None
        
    async def should_review_pr(self, pr_info: PRInfo) -> bool:
        """Determine if a PR should be reviewed based on commit SHA comparison."""
        repository = pr_info.repository_name
        pr_number = pr_info.number
        current_head_sha = pr_info.head_sha

        if not current_head_sha:
            logger.warning(f"PR #{pr_number} in {repository}: No head SHA available, skipping review")
            return False

        # Check if we have a completed review for this specific head SHA
        latest_review = await self.get_review_for_commit(repository, pr_number, current_head_sha)

        if latest_review:
            # If we've already completed a review for this exact commit, check the action
            action = latest_review.review_action
            logger.info(f"PR #{pr_number} in {repository}: Already reviewed head SHA {current_head_sha[:8]} with action '{action.value}'")

            # Don't review again if we've taken any action on this exact commit
            # (including human reviews - they should only be re-evaluated when SHA changes)
            if action in [ReviewAction.APPROVE_WITH_COMMENT,
                         ReviewAction.APPROVE_WITHOUT_COMMENT,
                         ReviewAction.REQUEST_CHANGES,
                         ReviewAction.REQUIRES_HUMAN_REVIEW]:
                logger.info(f"PR #{pr_number} in {repository}: Skipping - already processed this commit with action '{action.value}'")
                return False

        # Check if we have a pending approval for this specific head SHA
        pending_approval = await self.get_pending_approval_for_commit(repository, pr_number, current_head_sha)

        if pending_approval:
            logger.info(f"PR #{pr_number} in {repository}: Already has pending approval for head SHA {current_head_sha[:8]}, skipping review")
            return False

        # No review or pending approval found for this commit, should review
        logger.info(f"PR #{pr_number} in {repository}: No review found for head SHA {current_head_sha[:8]}, will review")
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

        # Ensure any prior pending approvals for older commits are marked as expired
        expired_ids = self._expire_pending_approvals_for_pr_sync(pr_info)
        if expired_ids:
            logger.debug(
                "Expired prior pending approval IDs %s before creating new entry for PR #%s",
                expired_ids,
                pr_info.number,
            )

        # Check if this exact commit already has a pending approval (duplicate prevention)
        existing_commit = self._get_pending_approval_for_commit_sync(
            pr_info.repository_name, pr_info.number, pr_info.head_sha
        )

        if existing_commit:
            logger.info(f"Pending approval already exists for PR #{pr_info.number} commit {pr_info.head_sha[:8]}, returning existing ID: {existing_commit['id']}")
            return existing_commit['id']

        # Delete any expired records for the same commit (allows re-review after expiration)
        cursor.execute("""
            DELETE FROM pending_approvals
            WHERE repository = ? AND pr_number = ? AND head_sha = ? AND status = ?
        """, (pr_info.repository_name, pr_info.number, pr_info.head_sha, PENDING_APPROVAL_STATUS_EXPIRED))
        if cursor.rowcount > 0:
            logger.info(
                f"Deleted {cursor.rowcount} expired pending approval(s) for PR #{pr_info.number} "
                f"commit {pr_info.head_sha[:8]} to allow re-review"
            )

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
                INSERT INTO pending_approvals 
                (repository, pr_number, pr_title, pr_author, pr_url, review_action,
                 review_comment, review_summary, review_reason, inline_comments, 
                 inline_comments_count, head_sha, base_sha, status)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
                pr_info.head_sha,
                pr_info.base_sha,
                PENDING_APPROVAL_STATUS_PENDING
            ))

            approval_id = cursor.lastrowid
            logger.info(f"Created pending approval for PR #{pr_info.number} in {pr_info.repository_name} (ID: {approval_id})")
            return approval_id

        except sqlite3.Error as e:
            logger.error(f"Error creating pending approval: {e}")
            raise
            
    async def get_pending_approvals(self, status: str = PENDING_APPROVAL_STATUS_PENDING) -> List[Dict[str, Any]]:
        """Get pending approvals by status."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_pending_approvals_sync, status
        )

    def _get_pending_approvals_sync(self, status: str = PENDING_APPROVAL_STATUS_PENDING) -> List[Dict[str, Any]]:
        """Synchronous implementation of get_pending_approvals."""
        conn = self._get_connection()
        cursor = conn.cursor()

        if status not in VALID_PENDING_APPROVAL_STATUSES:
            raise ValueError(f"Invalid pending approval status requested: {status}")

        cursor.execute("""
            SELECT * FROM pending_approvals 
            WHERE status = ?
            ORDER BY created_at DESC
        """, (status,))

        approvals = []
        for row in cursor.fetchall():
            approval = dict(row)
            # Parse inline comments JSON - use edited version if available
            inline_comments_json = approval['edited_inline_comments'] or approval['inline_comments']
            if inline_comments_json:
                approval['inline_comments'] = json.loads(inline_comments_json)
            else:
                approval['inline_comments'] = []
            
            # Use edited versions for display if they exist, handle deletions properly
            if approval['edited_review_comment'] is not None:
                approval['display_review_comment'] = approval['edited_review_comment']
            else:
                approval['display_review_comment'] = approval['review_comment']
                
            if approval['edited_review_summary'] is not None:
                approval['display_review_summary'] = approval['edited_review_summary']
            else:
                approval['display_review_summary'] = approval['review_summary']
            
            approvals.append(approval)

        return approvals

    async def expire_pending_approvals_for_pr(self, pr_info: PRInfo) -> List[int]:
        """Mark existing pending approvals for a PR as expired when a new commit arrives."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._expire_pending_approvals_for_pr_sync, pr_info
        )

    def _expire_pending_approvals_for_pr_sync(self, pr_info: PRInfo) -> List[int]:
        conn = self._get_connection()
        cursor = conn.cursor()

        cursor.execute(
            """
            SELECT id, head_sha FROM pending_approvals
            WHERE repository = ? AND pr_number = ? AND status = ?
            """,
            (pr_info.repository_name, pr_info.number, PENDING_APPROVAL_STATUS_PENDING),
        )

        rows = cursor.fetchall()
        if not rows:
            return []

        current_head = pr_info.head_sha
        ids_to_expire = [row['id'] for row in rows if row['head_sha'] != current_head]
        duplicate_ids = [row['id'] for row in rows if row['head_sha'] == current_head]

        if duplicate_ids:
            logger.debug(
                "Pending approvals already exist for PR #%s in %s at head %s; keeping IDs %s",
                pr_info.number,
                pr_info.repository_name,
                current_head[:8] if current_head else 'unknown',
                duplicate_ids,
            )

        if not ids_to_expire:
            return []

        placeholders = ','.join('?' for _ in ids_to_expire)
        cursor.execute(
            f"UPDATE pending_approvals SET status = ? WHERE id IN ({placeholders})",
            [PENDING_APPROVAL_STATUS_EXPIRED, *ids_to_expire],
        )

        logger.info(
            "Expired pending approvals %s for PR #%s in %s due to new head %s",
            ids_to_expire,
            pr_info.number,
            pr_info.repository_name,
            current_head[:8] if current_head else 'unknown',
        )

        return ids_to_expire

    async def get_pending_approval_refs(self) -> List[Dict[str, Any]]:
        """Get lightweight metadata for pending approvals."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_pending_approval_refs_sync
        )

    def _get_pending_approval_refs_sync(self) -> List[Dict[str, Any]]:
        """Synchronous implementation returning minimal pending approval data."""
        conn = self._get_connection()
        cursor = conn.cursor()

        cursor.execute("""
            SELECT id, repository, pr_number, pr_title, pr_author, pr_url, head_sha, status, created_at
            FROM pending_approvals
            WHERE status = ?
            ORDER BY created_at DESC
        """, (PENDING_APPROVAL_STATUS_PENDING,))

        return [dict(row) for row in cursor.fetchall()]

    async def get_latest_pending_approval_for_pr(self, repository: str, pr_number: int) -> Optional[Dict[str, Any]]:
        """Fetch the most recent pending approval for a PR."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_latest_pending_approval_for_pr_sync, repository, pr_number
        )

    def _get_latest_pending_approval_for_pr_sync(self, repository: str, pr_number: int) -> Optional[Dict[str, Any]]:
        conn = self._get_connection()
        cursor = conn.cursor()

        cursor.execute(
            """
            SELECT * FROM pending_approvals
            WHERE repository = ? AND pr_number = ? AND status = ?
            ORDER BY created_at DESC
            LIMIT 1
            """,
            (repository, pr_number, PENDING_APPROVAL_STATUS_PENDING),
        )

        row = cursor.fetchone()
        if not row:
            return None

        approval = dict(row)
        inline_json = approval.get('edited_inline_comments') or approval.get('inline_comments')
        if inline_json:
            approval['inline_comments'] = json.loads(inline_json)
        else:
            approval['inline_comments'] = []

        if approval.get('edited_review_comment') is not None:
            approval['display_review_comment'] = approval['edited_review_comment']
        else:
            approval['display_review_comment'] = approval.get('review_comment')

        if approval.get('edited_review_summary') is not None:
            approval['display_review_summary'] = approval['edited_review_summary']
        else:
            approval['display_review_summary'] = approval.get('review_summary')

        return approval

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

        if status not in VALID_PENDING_APPROVAL_STATUSES:
            raise ValueError(f"Invalid pending approval status: {status}")

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
            # Parse inline comments JSON - use edited version if available
            inline_comments_json = approval['edited_inline_comments'] or approval['inline_comments']
            if inline_comments_json:
                approval['inline_comments'] = json.loads(inline_comments_json)
            else:
                approval['inline_comments'] = []
            
            # Use edited versions for display if they exist, handle deletions properly
            if approval['edited_review_comment'] is not None:
                approval['display_review_comment'] = approval['edited_review_comment']
            else:
                approval['display_review_comment'] = approval['review_comment']
                
            if approval['edited_review_summary'] is not None:
                approval['display_review_summary'] = approval['edited_review_summary']
            else:
                approval['display_review_summary'] = approval['review_summary']
            
            return approval
        return None

    async def update_approval_comment(self, approval_id: int, new_comment: str) -> bool:
        """Update the edited review comment for a pending approval."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._update_approval_comment_sync, approval_id, new_comment
        )

    def _update_approval_comment_sync(self, approval_id: int, new_comment: str) -> bool:
        """Synchronous implementation of update_approval_comment."""
        conn = self._get_connection()
        cursor = conn.cursor()

        try:
            cursor.execute("""
                UPDATE pending_approvals 
                SET edited_review_comment = ? 
                WHERE id = ?
            """, (new_comment, approval_id))

            if cursor.rowcount > 0:
                logger.info(f"Updated comment for pending approval {approval_id}")
                return True
            else:
                logger.warning(f"No pending approval found with ID: {approval_id}")
                return False

        except sqlite3.Error as e:
            logger.error(f"Error updating approval comment: {e}")
            raise

    async def update_approval_summary(self, approval_id: int, new_summary: str) -> bool:
        """Update the edited review summary for a pending approval."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._update_approval_summary_sync, approval_id, new_summary
        )

    def _update_approval_summary_sync(self, approval_id: int, new_summary: str) -> bool:
        """Synchronous implementation of update_approval_summary."""
        conn = self._get_connection()
        cursor = conn.cursor()

        try:
            cursor.execute("""
                UPDATE pending_approvals 
                SET edited_review_summary = ? 
                WHERE id = ?
            """, (new_summary, approval_id))

            if cursor.rowcount > 0:
                logger.info(f"Updated summary for pending approval {approval_id}")
                return True
            else:
                logger.warning(f"No pending approval found with ID: {approval_id}")
                return False

        except sqlite3.Error as e:
            logger.error(f"Error updating approval summary: {e}")
            raise

    async def update_approval_inline_comment(self, approval_id: int, comment_index: int, new_message: str) -> bool:
        """Update a specific inline comment for a pending approval."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._update_approval_inline_comment_sync, approval_id, comment_index, new_message
        )

    def _update_approval_inline_comment_sync(self, approval_id: int, comment_index: int, new_message: str) -> bool:
        """Synchronous implementation of update_approval_inline_comment."""
        conn = self._get_connection()
        cursor = conn.cursor()

        try:
            # Get current approval data
            cursor.execute("""
                SELECT inline_comments, edited_inline_comments FROM pending_approvals 
                WHERE id = ?
            """, (approval_id,))
            
            row = cursor.fetchone()
            if not row:
                logger.warning(f"No pending approval found with ID: {approval_id}")
                return False

            # Use edited comments if they exist, otherwise use original
            current_comments_json = row['edited_inline_comments'] or row['inline_comments']
            current_comments = json.loads(current_comments_json) if current_comments_json else []

            if comment_index >= len(current_comments):
                logger.warning(f"Comment index {comment_index} out of range for approval {approval_id}")
                return False

            # Update the specific comment
            current_comments[comment_index]['message'] = new_message

            # Save the updated comments
            updated_comments_json = json.dumps(current_comments)
            cursor.execute("""
                UPDATE pending_approvals 
                SET edited_inline_comments = ? 
                WHERE id = ?
            """, (updated_comments_json, approval_id))

            if cursor.rowcount > 0:
                logger.info(f"Updated inline comment {comment_index} for pending approval {approval_id}")
                return True
            else:
                return False

        except sqlite3.Error as e:
            logger.error(f"Error updating inline comment: {e}")
            raise

    async def delete_approval_comment(self, approval_id: int) -> bool:
        """Delete the review comment for a pending approval."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._delete_approval_comment_sync, approval_id
        )

    def _delete_approval_comment_sync(self, approval_id: int) -> bool:
        """Synchronous implementation of delete_approval_comment."""
        conn = self._get_connection()
        cursor = conn.cursor()

        try:
            cursor.execute("""
                UPDATE pending_approvals 
                SET edited_review_comment = '' 
                WHERE id = ?
            """, (approval_id,))

            if cursor.rowcount > 0:
                logger.info(f"Deleted comment for pending approval {approval_id}")
                return True
            else:
                logger.warning(f"No pending approval found with ID: {approval_id}")
                return False

        except sqlite3.Error as e:
            logger.error(f"Error deleting approval comment: {e}")
            raise

    async def delete_approval_summary(self, approval_id: int) -> bool:
        """Delete the review summary for a pending approval."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._delete_approval_summary_sync, approval_id
        )

    def _delete_approval_summary_sync(self, approval_id: int) -> bool:
        """Synchronous implementation of delete_approval_summary."""
        conn = self._get_connection()
        cursor = conn.cursor()

        try:
            cursor.execute("""
                UPDATE pending_approvals 
                SET edited_review_summary = '' 
                WHERE id = ?
            """, (approval_id,))

            if cursor.rowcount > 0:
                logger.info(f"Deleted summary for pending approval {approval_id}")
                return True
            else:
                logger.warning(f"No pending approval found with ID: {approval_id}")
                return False

        except sqlite3.Error as e:
            logger.error(f"Error deleting approval summary: {e}")
            raise

    async def delete_approval_inline_comment(self, approval_id: int, comment_index: int) -> bool:
        """Delete a specific inline comment for a pending approval."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._delete_approval_inline_comment_sync, approval_id, comment_index
        )

    def _delete_approval_inline_comment_sync(self, approval_id: int, comment_index: int) -> bool:
        """Synchronous implementation of delete_approval_inline_comment."""
        conn = self._get_connection()
        cursor = conn.cursor()

        try:
            # Get current approval data
            cursor.execute("""
                SELECT inline_comments, edited_inline_comments FROM pending_approvals 
                WHERE id = ?
            """, (approval_id,))
            
            row = cursor.fetchone()
            if not row:
                logger.warning(f"No pending approval found with ID: {approval_id}")
                return False

            # Use edited comments if they exist, otherwise use original
            current_comments_json = row['edited_inline_comments'] or row['inline_comments']
            current_comments = json.loads(current_comments_json) if current_comments_json else []

            if comment_index >= len(current_comments):
                logger.warning(f"Comment index {comment_index} out of range for approval {approval_id}")
                return False

            # Remove the specific comment
            current_comments.pop(comment_index)

            # Save the updated comments
            updated_comments_json = json.dumps(current_comments)
            cursor.execute("""
                UPDATE pending_approvals 
                SET edited_inline_comments = ? 
                WHERE id = ?
            """, (updated_comments_json, approval_id))

            if cursor.rowcount > 0:
                logger.info(f"Deleted inline comment {comment_index} for pending approval {approval_id}")
                return True
            else:
                return False

        except sqlite3.Error as e:
            logger.error(f"Error deleting inline comment: {e}")
            raise
        
    async def get_approved_approvals(self, limit: int = 50) -> List[Dict[str, Any]]:
        """Get approved approvals history."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_approved_approvals_sync, limit
        )

    def _get_approved_approvals_sync(self, limit: int = 50) -> List[Dict[str, Any]]:
        """Synchronous implementation of get_approved_approvals."""
        conn = self._get_connection()
        cursor = conn.cursor()

        cursor.execute("""
            SELECT * FROM pending_approvals 
            WHERE status = ?
            ORDER BY created_at DESC
            LIMIT ?
        """, (PENDING_APPROVAL_STATUS_APPROVED, limit))

        approvals = []
        for row in cursor.fetchall():
            approval = dict(row)
            
            # Store both original and edited versions for comparison
            approval['original_review_comment'] = approval['review_comment']
            approval['original_review_summary'] = approval['review_summary']
            
            # Parse original inline comments for comparison (from the raw database field)
            original_comments_json = approval['inline_comments']  # Raw database field
            if original_comments_json:
                approval['original_inline_comments'] = json.loads(original_comments_json)
            else:
                approval['original_inline_comments'] = []
            
            # Parse inline comments JSON - use edited version if available for final display
            inline_comments_json = approval['edited_inline_comments'] or approval['inline_comments']
            if inline_comments_json:
                approval['inline_comments'] = json.loads(inline_comments_json)
            else:
                approval['inline_comments'] = []
            
            # Use edited versions for display if they exist, handle deletions properly
            if approval['edited_review_comment'] is not None:
                approval['final_review_comment'] = approval['edited_review_comment']
            else:
                approval['final_review_comment'] = approval['review_comment']
                
            if approval['edited_review_summary'] is not None:
                approval['final_review_summary'] = approval['edited_review_summary']
            else:
                approval['final_review_summary'] = approval['review_summary']
            
            approvals.append(approval)

        return approvals

    async def get_rejected_approvals(self, limit: int = 50) -> List[Dict[str, Any]]:
        """Get rejected approvals history."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_rejected_approvals_sync, limit
        )

    def _get_rejected_approvals_sync(self, limit: int = 50) -> List[Dict[str, Any]]:
        """Synchronous implementation of get_rejected_approvals."""
        conn = self._get_connection()
        cursor = conn.cursor()

        cursor.execute("""
            SELECT * FROM pending_approvals 
            WHERE status = ?
            ORDER BY created_at DESC
            LIMIT ?
        """, (PENDING_APPROVAL_STATUS_REJECTED, limit))

        approvals = []
        for row in cursor.fetchall():
            approval = dict(row)
            
            # Store both original and edited versions for comparison
            approval['original_review_comment'] = approval['review_comment']
            approval['original_review_summary'] = approval['review_summary']
            
            # Parse original inline comments for comparison (from the raw database field)
            original_comments_json = approval['inline_comments']  # Raw database field
            if original_comments_json:
                approval['original_inline_comments'] = json.loads(original_comments_json)
            else:
                approval['original_inline_comments'] = []
            
            # Parse inline comments JSON - use edited version if available for final display
            inline_comments_json = approval['edited_inline_comments'] or approval['inline_comments']
            if inline_comments_json:
                approval['inline_comments'] = json.loads(inline_comments_json)
            else:
                approval['inline_comments'] = []
            
            # Use edited versions for display if they exist, handle deletions properly
            if approval['edited_review_comment'] is not None:
                approval['final_review_comment'] = approval['edited_review_comment']
            else:
                approval['final_review_comment'] = approval['review_comment']
                
            if approval['edited_review_summary'] is not None:
                approval['final_review_summary'] = approval['edited_review_summary']
            else:
                approval['final_review_summary'] = approval['review_summary']
            
            approvals.append(approval)

        return approvals

    async def get_completed_reviews(self, limit: int = 50) -> List[ReviewRecord]:
        """Get completed PR reviews ordered by reviewed_at descending."""
        return await asyncio.get_event_loop().run_in_executor(
            None, self._get_completed_reviews_sync, limit
        )

    def _get_completed_reviews_sync(self, limit: int = 50) -> List[ReviewRecord]:
        """Synchronous implementation of get_completed_reviews."""
        conn = self._get_connection()
        cursor = conn.cursor()

        cursor.execute("""
            SELECT * FROM pr_reviews 
            ORDER BY reviewed_at DESC 
            LIMIT ?
        """, (limit,))

        return [ReviewRecord.from_db_row(dict(row)) for row in cursor.fetchall()]
        
    def close(self):
        """Close database connections."""
        if hasattr(self._local, 'connection'):
            self._local.connection.close()
            delattr(self._local, 'connection')
