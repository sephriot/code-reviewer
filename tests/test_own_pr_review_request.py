"""Tests for requesting a review on a pending own PR via the web endpoint."""

import asyncio
from unittest.mock import AsyncMock

import pytest

from code_reviewer.database import (
    OWN_PR_STATUS_PENDING,
    OWN_PR_STATUS_READY_FOR_MERGING,
    ReviewDatabase,
)
from code_reviewer.models import PRInfo, ReviewAction, ReviewResult
from code_reviewer.web_server import ReviewWebServer


class _StubRequest:
    """Minimal stand-in for fastapi.Request exposing only json()."""

    async def json(self):
        return {"user_context": ""}


def _route_endpoint(app, path: str, method: str):
    for route in app.routes:
        if getattr(route, "path", None) == path and method in (
            getattr(route, "methods", None) or set()
        ):
            return route.endpoint
    raise AssertionError(f"Route {method} {path} not found")


@pytest.mark.asyncio
async def test_request_review_transitions_pending_own_pr(tmp_path):
    db = ReviewDatabase(tmp_path / "reviews.db")
    pr_info = PRInfo(
        id=31,
        number=31,
        repository=["acme", "repo"],
        url="https://github.com/acme/repo/pull/31",
        title="PR 31",
        author="alice",
        head_sha="deadbeef31",
        base_sha="cafebabe31",
    )

    llm_integration = AsyncMock()
    llm_integration.review_in_progress = False
    llm_integration.active_review_target = None
    llm_integration.review_pr.return_value = ReviewResult(
        action=ReviewAction.APPROVE_WITHOUT_COMMENT,
        reason="ok",
    )

    server = ReviewWebServer(
        db,
        github_client=AsyncMock(),
        sound_notifier=None,
        llm_integration=llm_integration,
    )
    endpoint = _route_endpoint(
        server.app, "/api/own-prs/{pr_id}/review-again", "POST"
    )

    try:
        # Manual mode tracked the PR as pending without a review
        pending_id = await db.create_own_pr(pr_info, review_result=None)
        tracked = await db.get_own_pr_by_id(pending_id)
        assert tracked["status"] == OWN_PR_STATUS_PENDING

        response = await endpoint(pending_id, _StubRequest())
        assert response.status_code == 200

        # Let the background review task created by the endpoint finish
        background = [
            task for task in asyncio.all_tasks() if task is not asyncio.current_task()
        ]
        await asyncio.gather(*background)

        llm_integration.review_pr.assert_awaited_once()
        entries = await db.get_own_prs()
        assert len(entries) == 1
        assert entries[0]["status"] == OWN_PR_STATUS_READY_FOR_MERGING
        assert entries[0]["head_sha"] == pr_info.head_sha
        assert entries[0]["review_action"] == ReviewAction.APPROVE_WITHOUT_COMMENT.value
    finally:
        db.close()
