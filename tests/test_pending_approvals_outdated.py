#!/usr/bin/env python3
"""Tests for pending approval status transitions."""

import asyncio
import sys
from pathlib import Path

import pytest

# Ensure src/ is on path
sys.path.insert(0, str(Path(__file__).parent.parent / 'src'))

from code_reviewer.database import (  # noqa: E402
    ReviewDatabase,
    PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED,
    PENDING_APPROVAL_STATUS_PENDING,
    PENDING_APPROVAL_STATUS_EXPIRED,
)
from code_reviewer.models import PRInfo, ReviewResult, ReviewAction  # noqa: E402


def _create_sample_pr_info(head_suffix: str) -> PRInfo:
    """Helper to create consistent PR metadata for tests."""
    return PRInfo(
        id=1,
        number=42,
        repository=["owner", "repo"],
        url="https://github.com/owner/repo/pull/42",
        title="Example PR",
        author="alice",
        head_sha=f"deadbeef{head_suffix}",
        base_sha="cafebabe0000",
    )


def _create_review_result() -> ReviewResult:
    """Helper to create a simple approve-with-comment result."""
    return ReviewResult(
        action=ReviewAction.APPROVE_WITH_COMMENT,
        comment="Looks good",
        summary="Approve with optional note",
        reason="Automated approval",
        comments=[],
    )


def test_pending_approval_marked_merged_or_closed(tmp_path: Path) -> None:
    """Pending approvals should move to MERGED_OR_CLOSED when status is updated."""
    db_path = tmp_path / "test_reviews.sqlite"
    database = ReviewDatabase(db_path)

    try:
        approval_id = asyncio.run(
            database.create_pending_approval(
                _create_sample_pr_info("01"),
                _create_review_result(),
            )
        )

        refs = asyncio.run(database.get_pending_approval_refs())
        assert len(refs) == 1
        assert refs[0]["id"] == approval_id
        assert refs[0]["head_sha"].endswith("01")

        # Invalid statuses should be rejected
        with pytest.raises(ValueError):
            asyncio.run(
                database.update_pending_approval_status(approval_id, "invalid")
            )

        asyncio.run(
            database.update_pending_approval_status(
                approval_id, PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED
            )
        )

        # No pending approvals should remain once marked merged/closed
        pending_after = asyncio.run(database.get_pending_approval_refs())
        assert pending_after == []

        outdated = asyncio.run(
            database.get_pending_approvals(PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED)
        )
        assert len(outdated) == 1
        assert outdated[0]["status"] == PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED
        assert outdated[0]["id"] == approval_id

        # Ensure status change did not mutate original comment fields
        assert outdated[0]["review_comment"] == "Looks good"
        assert outdated[0]["display_review_comment"] == "Looks good"
        assert outdated[0]["review_summary"] == "Approve with optional note"

    finally:
        database.close()


def test_pending_refs_only_return_live_entries(tmp_path: Path) -> None:
    """Pending refs accessor should ignore non-pending statuses."""
    db_path = tmp_path / "test_reviews.sqlite"
    database = ReviewDatabase(db_path)

    try:
        approval_pending = asyncio.run(
            database.create_pending_approval(
                _create_sample_pr_info("02"),
                _create_review_result(),
            )
        )

        # Manually mark to ensure mix of statuses
        asyncio.run(
                database.update_pending_approval_status(
                    approval_pending, PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED
                )
        )

        # Insert a fresh pending approval (different head SHA)
        asyncio.run(
            database.create_pending_approval(
                _create_sample_pr_info("03"),
                _create_review_result(),
            )
        )

        refs = asyncio.run(database.get_pending_approval_refs())
        assert len(refs) == 1
        assert refs[0]["status"] == PENDING_APPROVAL_STATUS_PENDING
        assert refs[0]["head_sha"].endswith("03")

    finally:
        database.close()


def test_expire_pending_approvals_for_new_commit(tmp_path: Path) -> None:
    """Pending approvals should transition to EXPIRED when a new commit arrives."""
    db_path = tmp_path / "test_reviews.sqlite"
    database = ReviewDatabase(db_path)

    try:
        # Original pending approval for commit deadbeef01
        original_pr = _create_sample_pr_info("01")
        approval_id = asyncio.run(
            database.create_pending_approval(
                original_pr,
                _create_review_result(),
            )
        )

        # Simulate new commit
        new_pr = _create_sample_pr_info("02")
        expired_ids = asyncio.run(database.expire_pending_approvals_for_pr(new_pr))

        assert approval_id in expired_ids

        expired_entries = asyncio.run(
            database.get_pending_approvals(PENDING_APPROVAL_STATUS_EXPIRED)
        )
        assert len(expired_entries) == 1
        assert expired_entries[0]["id"] == approval_id
        assert expired_entries[0]["status"] == PENDING_APPROVAL_STATUS_EXPIRED

    finally:
        database.close()
