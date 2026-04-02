#!/usr/bin/env python3
"""Tests for analytics database methods."""

import asyncio
import sys
import tempfile
from pathlib import Path
from datetime import datetime, timezone

import pytest

sys.path.insert(0, str(Path(__file__).parent.parent / "src"))

from code_reviewer.database import ReviewDatabase
from code_reviewer.models import PRInfo, ReviewResult, ReviewAction


@pytest.fixture
def db():
    """Create a temporary database for testing."""
    with tempfile.NamedTemporaryFile(suffix=".db", delete=False) as f:
        db_path = Path(f.name)
    database = ReviewDatabase(db_path)
    yield database
    database.close()
    db_path.unlink(missing_ok=True)


@pytest.fixture
def sample_pr_info():
    """Create a sample PRInfo for testing."""
    return PRInfo(
        id=1,
        number=123,
        repository=["owner", "repo"],
        url="https://github.com/owner/repo/pull/123",
        title="Test PR",
        author="testuser",
        head_sha="abc123def456",
        base_sha="base123",
    )


@pytest.mark.asyncio
async def test_get_analytics_overview_empty(db):
    """Test analytics overview with no reviews."""
    overview = await db.get_analytics_overview()

    assert overview["total_reviews"] == 0
    assert overview["avg_inline_comments"] == 0
    assert overview["human_review_rate"] == 0
    assert overview["approval_rate"] == 0


@pytest.mark.asyncio
async def test_get_analytics_overview_with_reviews(db, sample_pr_info):
    """Test analytics overview with reviews."""
    result1 = ReviewResult(action=ReviewAction.APPROVE_WITHOUT_COMMENT)
    result2 = ReviewResult(action=ReviewAction.APPROVE_WITH_COMMENT, comment="LGTM")
    result3 = ReviewResult(action=ReviewAction.REQUIRES_HUMAN_REVIEW, reason="Complex")

    await db.record_review(sample_pr_info, result1)

    pr2 = PRInfo(
        id=2,
        number=124,
        repository=["owner", "repo2"],
        url="https://github.com/owner/repo2/pull/124",
        title="PR 2",
        author="user2",
        head_sha="sha2",
        base_sha="base",
    )
    await db.record_review(pr2, result2)

    pr3 = PRInfo(
        id=3,
        number=125,
        repository=["owner", "repo"],
        url="https://github.com/owner/repo/pull/125",
        title="PR 3",
        author="user3",
        head_sha="sha3",
        base_sha="base",
    )
    await db.record_review(pr3, result3)

    overview = await db.get_analytics_overview()

    assert overview["total_reviews"] == 3
    assert overview["unique_repositories"] == 2
    assert overview["unique_authors"] == 3
    assert overview["approval_rate"] == pytest.approx(66.7, rel=0.1)
    assert overview["human_review_rate"] == pytest.approx(33.3, rel=0.1)


@pytest.mark.asyncio
async def test_get_action_distribution(db, sample_pr_info):
    """Test action distribution calculation."""
    result1 = ReviewResult(action=ReviewAction.APPROVE_WITHOUT_COMMENT)
    result2 = ReviewResult(action=ReviewAction.APPROVE_WITH_COMMENT)
    result3 = ReviewResult(action=ReviewAction.REQUEST_CHANGES)

    await db.record_review(sample_pr_info, result1)

    pr2 = PRInfo(
        id=2,
        number=124,
        repository=["owner", "repo"],
        url="https://github.com/owner/repo/pull/124",
        title="PR 2",
        author="user2",
        head_sha="sha2",
        base_sha="base",
    )
    await db.record_review(pr2, result2)

    pr3 = PRInfo(
        id=3,
        number=125,
        repository=["owner", "repo"],
        url="https://github.com/owner/repo/pull/125",
        title="PR 3",
        author="user3",
        head_sha="sha3",
        base_sha="base",
    )
    await db.record_review(pr3, result3)

    distribution = await db.get_action_distribution()

    assert distribution["total"] == 3
    assert "approve_with_comment" in distribution["distribution"]
    assert "approve_without_comment" in distribution["distribution"]
    assert "request_changes" in distribution["distribution"]
    assert distribution["distribution"]["approve_with_comment"]["count"] == 1
    assert distribution["distribution"]["approve_with_comment"][
        "percentage"
    ] == pytest.approx(33.3, rel=0.1)


@pytest.mark.asyncio
async def test_get_repository_stats(db, sample_pr_info):
    """Test repository statistics."""
    result = ReviewResult(action=ReviewAction.APPROVE_WITH_COMMENT, comment="Good")

    await db.record_review(sample_pr_info, result)

    pr2 = PRInfo(
        id=2,
        number=124,
        repository=["owner", "repo"],
        url="https://github.com/owner/repo/pull/124",
        title="PR 2",
        author="user2",
        head_sha="sha2",
        base_sha="base",
    )
    await db.record_review(pr2, result)

    pr3 = PRInfo(
        id=3,
        number=125,
        repository=["other", "repo"],
        url="https://github.com/other/repo/pull/125",
        title="PR 3",
        author="user3",
        head_sha="sha3",
        base_sha="base",
    )
    result3 = ReviewResult(action=ReviewAction.REQUIRES_HUMAN_REVIEW, reason="Complex")
    await db.record_review(pr3, result3)

    stats = await db.get_repository_stats()

    assert len(stats) == 2
    assert stats[0]["repository"] == "owner/repo"
    assert stats[0]["total_reviews"] == 2
    assert stats[0]["approval_rate"] == 100.0


@pytest.mark.asyncio
async def test_get_author_stats(db, sample_pr_info):
    """Test author statistics."""
    result = ReviewResult(action=ReviewAction.APPROVE_WITH_COMMENT)

    await db.record_review(sample_pr_info, result)

    pr2 = PRInfo(
        id=2,
        number=124,
        repository=["owner", "repo"],
        url="https://github.com/owner/repo/pull/124",
        title="PR 2",
        author="testuser",
        head_sha="sha2",
        base_sha="base",
    )
    await db.record_review(pr2, result)

    pr3 = PRInfo(
        id=3,
        number=125,
        repository=["owner", "repo"],
        url="https://github.com/owner/repo/pull/125",
        title="PR 3",
        author="otheruser",
        head_sha="sha3",
        base_sha="base",
    )
    result3 = ReviewResult(action=ReviewAction.REQUEST_CHANGES)
    await db.record_review(pr3, result3)

    stats = await db.get_author_stats()

    assert len(stats) == 2
    testuser_stat = next(s for s in stats if s["pr_author"] == "testuser")
    assert testuser_stat["total_reviews"] == 2
    assert testuser_stat["approval_rate"] == 100.0


@pytest.mark.asyncio
async def test_get_reviews_by_day_empty(db):
    """Test daily review trends with no data."""
    trends = await db.get_reviews_by_day(30)

    assert trends == []


@pytest.mark.asyncio
async def test_get_reviews_by_day_with_data(db, sample_pr_info):
    """Test daily review trends with data."""
    result = ReviewResult(action=ReviewAction.APPROVE_WITHOUT_COMMENT)
    await db.record_review(sample_pr_info, result)

    trends = await db.get_reviews_by_day(30)

    assert len(trends) >= 1
    assert "date" in trends[0]
    assert "total" in trends[0]
    assert trends[0]["total"] >= 1


@pytest.mark.asyncio
async def test_get_pending_approval_stats_empty(db):
    """Test pending approval stats with no data."""
    stats = await db.get_pending_approval_stats()

    assert stats["total"] == 0
    assert stats["approval_rate"] == 0
    assert stats["decided"] == 0


if __name__ == "__main__":
    pytest.main([__file__, "-v"])
