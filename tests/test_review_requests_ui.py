"""Tests for the unfiltered review-request queue and manual review action."""

import asyncio
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock

import pytest

from code_reviewer.database import (
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
async def test_review_requests_endpoint_fetches_without_automatic_review_filters(
    tmp_path,
):
    db = ReviewDatabase(tmp_path / "reviews.db")
    github_client = AsyncMock()
    github_client.get_review_requests.return_value = [_pr()]
    config = SimpleNamespace(
        github_username="reviewer",
        repositories=["selected/repository"],
        pr_authors=["selected-author"],
    )
    server = ReviewWebServer(db, github_client, config=config)
    endpoint = _route_endpoint(server.app, "/api/review-requests", "GET")

    try:
        response = await endpoint()

        github_client.get_review_requests.assert_awaited_once_with("reviewer")
        assert b'"repository":"acme/widgets"' in response.body
        assert b'"review_state":"not_reviewed"' in response.body
    finally:
        db.close()


@pytest.mark.asyncio
async def test_requested_review_runs_live_pr_through_monitor(tmp_path):
    db = ReviewDatabase(tmp_path / "reviews.db")
    pr_info = _pr()
    github_client = AsyncMock()
    github_client.get_review_requests.return_value = [pr_info]
    llm_integration = Mock()
    llm_integration.model = ReviewModel.CLAUDE
    llm_integration.review_in_progress = False
    llm_integration.active_review_target = None
    llm_integration.resolve_claude_model.return_value = "sonnet"
    monitor = Mock()
    monitor.review_pr_on_demand = AsyncMock()
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
        response = await endpoint(
            _StubRequest(
                {
                    "repository": "acme/widgets",
                    "pr_number": 42,
                    "user_context": "focus on concurrency",
                    "claude_model": "sonnet",
                }
            )
        )
        await asyncio.sleep(0)

        assert response.status_code == 200
        monitor.review_pr_on_demand.assert_awaited_once_with(
            pr_info,
            user_context="focus on concurrency",
            claude_model="sonnet",
        )
    finally:
        db.close()


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
    github_client = AsyncMock()
    github_client.get_review_requests.return_value = [pr_info]
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
