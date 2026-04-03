import asyncio
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from code_reviewer.database import (
    OWN_PR_STATUS_EXPIRED,
    OWN_PR_STATUS_NEEDS_ATTENTION,
    OWN_PR_STATUS_READY_FOR_MERGING,
)
from code_reviewer.github_monitor import GitHubMonitor
from code_reviewer.llm_integration import LLMIntegration
from code_reviewer.models import PRInfo, ReviewAction, ReviewModel, ReviewResult


def _make_pr_info(number: int) -> PRInfo:
    return PRInfo(
        id=number,
        number=number,
        repository=["acme", "repo"],
        url=f"https://github.com/acme/repo/pull/{number}",
        title=f"PR {number}",
        author="alice",
        head_sha=f"deadbeef{number:02d}",
        base_sha=f"cafebabe{number:02d}",
    )


def _make_monitor_config(tmp_path: Path) -> SimpleNamespace:
    return SimpleNamespace(
        sound_enabled=False,
        sound_file=None,
        approval_sound_enabled=False,
        approval_sound_file=None,
        timeout_sound_enabled=False,
        timeout_sound_file=None,
        merged_or_closed_sound_enabled=False,
        merged_or_closed_sound_file=None,
        own_pr_ready_sound_enabled=False,
        own_pr_ready_sound_file=None,
        own_pr_needs_attention_sound_enabled=False,
        own_pr_needs_attention_sound_file=None,
        review_started_sound_enabled=False,
        review_started_sound_file=None,
        database_path=tmp_path / "reviews.db",
        dry_run=False,
        repositories=[],
        pr_authors=[],
        poll_interval=1,
        own_pr_enabled=False,
        review_started_comment_enabled=False,
        review_timeout=None,
        review_model=ReviewModel.CLAUDE,
        github_username="alice",
    )


@pytest.mark.asyncio
async def test_review_pr_serializes_concurrent_requests(monkeypatch):
    integration = LLMIntegration(Path("prompt.txt"), ReviewModel.CLAUDE)
    active_reviews = 0
    max_active_reviews = 0

    async def fake_run_model_cli(self, pr_info, previous_pending, *, user_context=None):
        nonlocal active_reviews, max_active_reviews
        active_reviews += 1
        max_active_reviews = max(max_active_reviews, active_reviews)
        await asyncio.sleep(0.05)
        active_reviews -= 1
        return '{"action":"approve_without_comment","reason":"ok"}'

    monkeypatch.setattr(LLMIntegration, "_run_model_cli", fake_run_model_cli)
    monkeypatch.setattr(
        LLMIntegration,
        "_parse_review_result",
        lambda self, result: ReviewResult(
            action=ReviewAction.APPROVE_WITHOUT_COMMENT,
            reason="ok",
        ),
    )

    await asyncio.gather(
        integration.review_pr(_make_pr_info(1)),
        integration.review_pr(_make_pr_info(2)),
    )

    assert max_active_reviews == 1
    assert integration.review_in_progress is False
    assert integration.active_review_target is None


@pytest.mark.asyncio
async def test_check_for_new_prs_skips_poll_when_review_in_progress(tmp_path):
    github_client = AsyncMock()
    llm_integration = AsyncMock()
    llm_integration.review_in_progress = True
    llm_integration.active_review_target = "acme/repo#7"

    monitor = GitHubMonitor(
        github_client,
        llm_integration,
        _make_monitor_config(tmp_path),
    )

    try:
        await monitor._check_for_new_prs()
        github_client.get_review_requests.assert_not_awaited()
    finally:
        monitor.db.close()


@pytest.mark.asyncio
async def test_check_for_own_prs_skips_same_head_sha_after_tracking(tmp_path):
    github_client = AsyncMock()
    pr_info = _make_pr_info(11)
    github_client.get_own_prs.return_value = [pr_info]
    github_client.get_pr_status.return_value = {"state": "open", "merged": False}

    llm_integration = AsyncMock()
    llm_integration.review_in_progress = False
    llm_integration.active_review_target = None
    llm_integration.review_pr.return_value = ReviewResult(
        action=ReviewAction.APPROVE_WITHOUT_COMMENT,
        reason="ok",
    )

    monitor = GitHubMonitor(
        github_client,
        llm_integration,
        _make_monitor_config(tmp_path),
    )
    monitor.sound_notifier.play_review_started_sound = AsyncMock()
    monitor.sound_notifier.play_pr_ready_sound = AsyncMock()
    monitor.sound_notifier.play_pr_needs_attention_sound = AsyncMock()

    try:
        await monitor._check_for_own_prs()
        await monitor._check_for_own_prs()

        llm_integration.review_pr.assert_awaited_once_with(
            pr_info, timeout=None
        )
        tracked = await monitor.db.get_own_pr_by_commit(
            pr_info.repository_name, pr_info.number, pr_info.head_sha
        )
        assert tracked is not None
        assert tracked["status"] == OWN_PR_STATUS_READY_FOR_MERGING
    finally:
        monitor.db.close()


@pytest.mark.asyncio
async def test_check_for_own_prs_reviews_new_head_sha_and_expires_old_one(tmp_path):
    github_client = AsyncMock()
    first_pr = _make_pr_info(13)
    second_pr = PRInfo(
        id=first_pr.id,
        number=first_pr.number,
        repository=first_pr.repository,
        url=first_pr.url,
        title=first_pr.title,
        author=first_pr.author,
        head_sha="feedface13",
        base_sha=first_pr.base_sha,
    )
    github_client.get_own_prs.side_effect = [[first_pr], [second_pr]]
    github_client.get_pr_status.return_value = {"state": "open", "merged": False}

    llm_integration = AsyncMock()
    llm_integration.review_in_progress = False
    llm_integration.active_review_target = None
    llm_integration.review_pr.side_effect = [
        ReviewResult(action=ReviewAction.APPROVE_WITHOUT_COMMENT, reason="first"),
        ReviewResult(action=ReviewAction.REQUEST_CHANGES, reason="second"),
    ]

    monitor = GitHubMonitor(
        github_client,
        llm_integration,
        _make_monitor_config(tmp_path),
    )
    monitor.sound_notifier.play_review_started_sound = AsyncMock()
    monitor.sound_notifier.play_pr_ready_sound = AsyncMock()
    monitor.sound_notifier.play_pr_needs_attention_sound = AsyncMock()

    try:
        await monitor._check_for_own_prs()
        await monitor._check_for_own_prs()

        assert llm_integration.review_pr.await_count == 2
        old_entry = await monitor.db.get_own_pr_by_commit(
            first_pr.repository_name, first_pr.number, first_pr.head_sha
        )
        new_entry = await monitor.db.get_own_pr_by_commit(
            second_pr.repository_name, second_pr.number, second_pr.head_sha
        )

        assert old_entry is not None
        assert old_entry["status"] == OWN_PR_STATUS_EXPIRED
        assert new_entry is not None
        assert new_entry["status"] == OWN_PR_STATUS_NEEDS_ATTENTION
    finally:
        monitor.db.close()
