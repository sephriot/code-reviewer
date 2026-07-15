"""Tests for the unfiltered review-request queue and manual review action."""

import asyncio
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock

import pytest

from code_reviewer.database import (
    PENDING_APPROVAL_STATUS_EXPIRED,
    PENDING_APPROVAL_STATUS_REJECTED,
    ReviewDatabase,
)
from code_reviewer.models import PRInfo, ReviewAction, ReviewModel, ReviewResult
from code_reviewer.web_server import ReviewWebServer


class _StubRequest:
    def __init__(self, body):
        self.body = body

    async def json(self):
        return self.body


def _route_endpoint(app, path: str, method: str):
    for route in app.routes:
        if getattr(route, "path", None) == path and method in (
            getattr(route, "methods", None) or set()
        ):
            return route.endpoint
    raise AssertionError(f"Route {method} {path} not found")


def _pr() -> PRInfo:
    return PRInfo(
        id=123,
        number=42,
        repository=["acme", "widgets"],
        url="https://github.com/acme/widgets/pull/42",
        title="Improve review queue",
        author="alice",
        head_sha="deadbeef42",
        base_sha="cafebabe42",
    )


@pytest.mark.asyncio
async def test_review_requests_endpoint_reads_cached_snapshot(tmp_path):
    db = ReviewDatabase(tmp_path / "reviews.db")
    await db.sync_review_requests([_pr()])
    github_client = AsyncMock()
    server = ReviewWebServer(db, github_client)
    endpoint = _route_endpoint(server.app, "/api/review-requests", "GET")

    try:
        response = await endpoint()

        github_client.get_review_requests.assert_not_awaited()
        assert b'"repository":"acme/widgets"' in response.body
        assert b'"review_state":"not_reviewed"' in response.body
        assert b'"last_synced_at":null' not in response.body
    finally:
        db.close()


@pytest.mark.asyncio
async def test_requested_review_returns_before_live_revalidation(tmp_path):
    db = ReviewDatabase(tmp_path / "reviews.db")
    pr_info = _pr()
    await db.sync_review_requests([pr_info])
    revalidation_started = asyncio.Event()
    release_revalidation = asyncio.Event()
    review_started = asyncio.Event()

    async def delayed_revalidation(*args):
        revalidation_started.set()
        await release_revalidation.wait()
        return pr_info

    async def mark_review_started(*args, **kwargs):
        review_started.set()

    github_client = AsyncMock()
    github_client.get_requested_pr.side_effect = delayed_revalidation
    llm_integration = Mock()
    llm_integration.model = ReviewModel.CLAUDE
    llm_integration.review_in_progress = False
    llm_integration.active_review_target = None
    llm_integration.resolve_claude_model.return_value = "sonnet"
    monitor = Mock()
    monitor.review_pr_on_demand = AsyncMock(side_effect=mark_review_started)
    config = SimpleNamespace(github_username="reviewer")
    server = ReviewWebServer(
        db,
        github_client,
        llm_integration=llm_integration,
        config=config,
        monitor=monitor,
    )
    endpoint = _route_endpoint(server.app, "/api/review-requests/review", "POST")

    try:
        response = await asyncio.wait_for(
            endpoint(
                _StubRequest(
                    {
                        "repository": "acme/widgets",
                        "pr_number": 42,
                        "user_context": "focus on concurrency",
                        "claude_model": "sonnet",
                    }
                )
            ),
            timeout=0.1,
        )

        assert response.status_code == 200
        await asyncio.wait_for(revalidation_started.wait(), timeout=0.1)
        monitor.review_pr_on_demand.assert_not_awaited()

        release_revalidation.set()
        await asyncio.wait_for(review_started.wait(), timeout=0.1)

        github_client.get_requested_pr.assert_awaited_once_with(
            "reviewer", "acme/widgets", 42
        )
        github_client.get_review_requests.assert_not_awaited()
        monitor.review_pr_on_demand.assert_awaited_once_with(
            pr_info,
            user_context="focus on concurrency",
            claude_model="sonnet",
        )
    finally:
        db.close()


@pytest.mark.asyncio
async def test_requested_review_uses_cached_pr_when_refresh_fails(tmp_path):
    db = ReviewDatabase(tmp_path / "reviews.db")
    pr_info = _pr()
    await db.sync_review_requests([pr_info])
    github_client = AsyncMock()
    github_client.get_requested_pr.side_effect = RuntimeError("GitHub unavailable")
    llm_integration = Mock(
        model=ReviewModel.CLAUDE,
        review_in_progress=False,
        active_review_target=None,
    )
    monitor = Mock()
    review_started = asyncio.Event()

    async def mark_review_started(*args, **kwargs):
        review_started.set()

    monitor.review_pr_on_demand = AsyncMock(side_effect=mark_review_started)
    server = ReviewWebServer(
        db,
        github_client,
        llm_integration=llm_integration,
        config=SimpleNamespace(github_username="reviewer"),
        monitor=monitor,
    )
    endpoint = _route_endpoint(server.app, "/api/review-requests/review", "POST")

    try:
        response = await endpoint(
            _StubRequest({"repository": "acme/widgets", "pr_number": 42})
        )
        await asyncio.wait_for(review_started.wait(), timeout=0.1)

        assert response.status_code == 200
        monitor.review_pr_on_demand.assert_awaited_once_with(
            pr_info,
            user_context=None,
            claude_model=None,
        )
    finally:
        db.close()


def test_requested_review_shows_starting_state_before_waiting_for_api():
    template = (
        Path(__file__).parents[1]
        / "src"
        / "code_reviewer"
        / "templates"
        / "dashboard.html"
    ).read_text()

    button_feedback = "startButton.textContent = 'Starting…';"
    request_start = "await fetch('/api/review-requests/review'"
    assert 'id="review-request-start-${pr.id}"' in template
    assert template.index(button_feedback) < template.index(request_start)


@pytest.mark.asyncio
async def test_review_request_ignores_archived_pending_decision(tmp_path):
    db = ReviewDatabase(tmp_path / "reviews.db")
    pr_info = _pr()
    pending_id = await db.create_pending_approval(
        pr_info,
        ReviewResult(action=ReviewAction.REQUEST_CHANGES, reason="needs work"),
    )
    await db.update_pending_approval_status(
        pending_id, PENDING_APPROVAL_STATUS_REJECTED, "not posting"
    )
    await db.sync_review_requests([pr_info])
    github_client = AsyncMock()
    server = ReviewWebServer(
        db,
        github_client,
        config=SimpleNamespace(github_username="reviewer"),
    )
    endpoint = _route_endpoint(server.app, "/api/review-requests", "GET")

    try:
        response = await endpoint()

        assert b'"review_state":"not_reviewed"' in response.body
    finally:
        db.close()


@pytest.mark.asyncio
async def test_approval_rejects_a_pending_review_for_a_new_commit(tmp_path):
    db = ReviewDatabase(tmp_path / "reviews.db")
    pr_info = _pr()
    approval_id = await db.create_pending_approval(
        pr_info,
        ReviewResult(
            action=ReviewAction.APPROVE_WITH_COMMENT,
            comment="Looks good",
        ),
    )
    github_client = AsyncMock()
    github_client.get_pr_status.return_value = {
        "state": "open",
        "merged": False,
        "head_sha": "feedface42",
        "base_sha": pr_info.base_sha,
    }
    server = ReviewWebServer(db, github_client)
    server._post_github_review = AsyncMock(return_value=True)
    endpoint = _route_endpoint(server.app, "/api/approvals/{approval_id}/approve", "POST")

    try:
        response = await endpoint(approval_id, _StubRequest({"comment": ""}))

        assert response.status_code == 409
        server._post_github_review.assert_not_awaited()
        approval = await db.get_pending_approval(approval_id)
        assert approval is not None
        assert approval["status"] == PENDING_APPROVAL_STATUS_EXPIRED
    finally:
        db.close()


@pytest.mark.asyncio
async def test_review_request_snapshot_replaces_requests_atomically(tmp_path):
    db = ReviewDatabase(tmp_path / "reviews.db")
    old_pr = _pr()
    current_pr = PRInfo(
        id=456,
        number=7,
        repository=["acme", "api"],
        url="https://github.com/acme/api/pull/7",
        title="Current request",
        author="bob",
        head_sha="deadbeef07",
        base_sha="cafebabe07",
    )

    try:
        assert await db.get_review_requests_last_synced_at() is None
        await db.sync_review_requests([old_pr, current_pr])
        assert await db.get_review_requests_last_synced_at() is not None
        await db.sync_review_requests([current_pr])

        requests = await db.get_review_requests()
        assert [request["repository"] for request in requests] == ["acme/api"]

        await db.sync_review_requests([])
        assert await db.get_review_requests() == []
    finally:
        db.close()
