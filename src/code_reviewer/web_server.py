"""Web UI server for managing PR reviews and approvals."""

import asyncio
import json
import logging
from pathlib import Path
from typing import Dict, Any, Optional, List

from fastapi import FastAPI, HTTPException, Request, Form
from fastapi.responses import HTMLResponse, JSONResponse
from fastapi.staticfiles import StaticFiles
from fastapi.templating import Jinja2Templates

from .database import (
    ReviewDatabase,
    PENDING_APPROVAL_STATUS_APPROVED,
    PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED,
    PENDING_APPROVAL_STATUS_PENDING,
    PENDING_APPROVAL_STATUS_REJECTED,
    PENDING_APPROVAL_STATUS_EXPIRED,
    OWN_PR_STATUS_PENDING,
    OWN_PR_STATUS_READY_FOR_MERGING,
    OWN_PR_STATUS_NEEDS_ATTENTION,
    OWN_PR_STATUS_MERGED,
    OWN_PR_STATUS_CLOSED,
)
from .github_client import GitHubClient
from .models import ReviewRecord, ReviewResult, ReviewAction, InlineComment, PRInfo


logger = logging.getLogger(__name__)


class ReviewWebServer:
    """Web server for PR review management."""

    def __init__(
        self,
        database: ReviewDatabase,
        github_client: GitHubClient,
        sound_notifier=None,
        llm_integration=None,
    ):
        self.database = database
        self.github_client = github_client
        self.sound_notifier = sound_notifier
        self.llm_integration = llm_integration
        self.app = FastAPI(title="Code Review Dashboard")

        # Setup static files and templates
        self.templates_dir = Path(__file__).parent / "templates"
        self.static_dir = Path(__file__).parent / "static"
        self.static_dir.mkdir(exist_ok=True)

        # Setup templates and static files
        self.templates = Jinja2Templates(directory=str(self.templates_dir))
        self.app.mount(
            "/static", StaticFiles(directory=str(self.static_dir)), name="static"
        )

        self._setup_routes()

    def _review_busy_response(self) -> JSONResponse:
        """Return a consistent response when another review is already running."""
        active_target = None
        if self.llm_integration:
            active_target = self.llm_integration.active_review_target

        message = "Another review is already in progress."
        if active_target:
            message = f"Another review is already in progress for {active_target}."

        return JSONResponse(
            status_code=409,
            content={"status": "busy", "message": message},
        )

    @staticmethod
    def _append_text_block(original: Optional[str], addition: str) -> str:
        """Append a formatted block of text with spacing."""
        addition_text = (addition or "").strip()
        if not addition_text:
            return (original or "").strip()

        original_text = (original or "").strip()
        if not original_text:
            return addition_text

        return f"{original_text}\n\n{addition_text}"

    def _setup_routes(self):
        """Setup FastAPI routes."""

        @self.app.get("/", response_class=HTMLResponse)
        async def dashboard(request: Request):
            """Main dashboard page."""
            return self.templates.TemplateResponse(
                "dashboard.html", {"request": request}
            )

        @self.app.get("/api/pending-approvals")
        async def get_pending_approvals():
            """Get all pending approvals."""
            try:
                approvals = await self.database.get_pending_approvals()
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(f"Failed to get pending approvals: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/own-prs")
        async def get_own_prs():
            """Get all own PRs."""
            try:
                prs = await self.database.get_own_prs()
                return JSONResponse(content=prs)
            except Exception as e:
                logger.error(f"Failed to get own PRs: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.delete("/api/own-prs/{pr_id}")
        async def delete_own_pr(pr_id: int):
            """Delete an own PR from tracking."""
            try:
                deleted = await self.database.delete_own_pr(pr_id)
                if deleted:
                    return JSONResponse(
                        content={"status": "success", "message": "Own PR deleted"}
                    )
                else:
                    raise HTTPException(status_code=404, detail="Own PR not found")
            except HTTPException:
                raise
            except Exception as e:
                logger.error(f"Failed to delete own PR: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.post("/api/own-prs/{pr_id}/review-again")
        async def review_own_pr_again(pr_id: int, request: Request):
            """Trigger a fresh review for an own PR with optional custom context."""
            try:
                if not self.llm_integration:
                    raise HTTPException(
                        status_code=500,
                        detail="LLM integration not available",
                    )

                user_context = None
                try:
                    body = await request.json()
                    user_context = body.get("user_context", "").strip() or None
                except Exception:
                    pass

                own_pr = await self.database.get_own_pr_by_id(pr_id)
                if not own_pr:
                    raise HTTPException(status_code=404, detail="Own PR not found")

                await self.database.delete_own_pr_by_commit(
                    own_pr["repository"], own_pr["pr_number"], own_pr["head_sha"]
                )

                pr_info = PRInfo(
                    id=own_pr["pr_number"],
                    number=own_pr["pr_number"],
                    repository=own_pr["repository"].split("/"),
                    url=own_pr["pr_url"],
                    title=own_pr["pr_title"],
                    author=own_pr["pr_author"],
                    head_sha=own_pr["head_sha"],
                    base_sha=own_pr["base_sha"],
                )

                if self.llm_integration.review_in_progress:
                    logger.info(
                        "Rejecting own PR re-review for %s#%s because %s is already running",
                        pr_info.repository_name,
                        pr_info.number,
                        self.llm_integration.active_review_target or "another review",
                    )
                    return self._review_busy_response()

                async def run_review():
                    try:
                        logger.info(
                            f"Starting re-review for own PR {pr_info.repository_name}#{pr_info.number}"
                            + (f" with user context" if user_context else "")
                        )
                        review_result = await self.llm_integration.review_pr(
                            pr_info, user_context=user_context
                        )
                        logger.info(
                            f"Re-review complete for own PR {pr_info.repository_name}#{pr_info.number}: "
                            f"{review_result.action.value}"
                        )

                        await self.database.create_own_pr(pr_info, review_result)

                        if review_result.action in (
                            ReviewAction.APPROVE_WITHOUT_COMMENT,
                            ReviewAction.APPROVE_WITH_COMMENT,
                        ):
                            if self.sound_notifier:
                                await self.sound_notifier.play_pr_ready_sound(
                                    {
                                        "repo": pr_info.repository_name,
                                        "pr_number": pr_info.number,
                                        "author": pr_info.author,
                                        "title": pr_info.title,
                                    }
                                )
                        else:
                            if self.sound_notifier:
                                await self.sound_notifier.play_pr_needs_attention_sound(
                                    {
                                        "repo": pr_info.repository_name,
                                        "pr_number": pr_info.number,
                                        "author": pr_info.author,
                                        "title": pr_info.title,
                                    }
                                )

                    except Exception as e:
                        logger.error(
                            f"Re-review failed for own PR {pr_info.repository_name}#{pr_info.number}: {e}"
                        )
                        failure_result = ReviewResult(
                            action=ReviewAction.REQUIRES_HUMAN_REVIEW,
                            reason=f"Re-review failed: {e}",
                        )
                        await self.database.create_own_pr(pr_info, failure_result)
                        if self.sound_notifier:
                            await self.sound_notifier.play_pr_needs_attention_sound(
                                {
                                    "repo": pr_info.repository_name,
                                    "pr_number": pr_info.number,
                                    "author": pr_info.author,
                                    "title": pr_info.title,
                                }
                            )

                asyncio.create_task(run_review())

                return JSONResponse(
                    content={
                        "status": "success",
                        "message": "Review triggered. Refresh in a moment to see results.",
                    }
                )

            except HTTPException:
                raise
            except Exception as e:
                logger.error(f"Failed to trigger re-review for own PR {pr_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/human-reviews")
        async def get_human_reviews():
            """Get PRs marked for human review."""
            try:
                reviews = await self.database.get_human_review_prs()
                return JSONResponse(
                    content=[
                        {
                            "id": review.id,
                            "repository": review.repository,
                            "pr_number": review.pr_number,
                            "pr_title": review.pr_title,
                            "pr_author": review.pr_author,
                            "review_reason": review.review_reason,
                            "reviewed_at": review.reviewed_at,
                            "head_sha": review.head_sha,
                            "base_sha": review.base_sha,
                        }
                        for review in reviews
                    ]
                )
            except Exception as e:
                logger.error(f"Failed to get human reviews: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.post("/api/approvals/{approval_id}/approve")
        async def approve_pr(approval_id: int, request: Request):
            """Approve a PR and post the review."""
            try:
                body = await request.json()
                user_comment = body.get("comment", "")
                logger.info(
                    f"Received approval request for ID {approval_id} with comment: '{user_comment}'"
                )

                # Get the pending approval
                approval = await self.database.get_pending_approval(approval_id)
                if not approval:
                    raise HTTPException(status_code=404, detail="Approval not found")

                if approval["status"] != PENDING_APPROVAL_STATUS_PENDING:
                    raise HTTPException(
                        status_code=400, detail="Approval is not pending"
                    )

                # Create PR info and review result objects
                pr_info = PRInfo(
                    id=0,  # Not used for this purpose
                    number=approval["pr_number"],
                    repository=approval["repository"].split("/"),
                    url=approval["pr_url"],
                    title=approval["pr_title"],
                    author=approval["pr_author"],
                )

                # Recreate inline comments
                inline_comments = [
                    InlineComment(
                        file=comment["file"],
                        line=comment["line"],
                        message=comment["message"],
                    )
                    for comment in approval["inline_comments"]
                ]

                # Use edited versions if available, otherwise use originals
                # If user_comment is provided from modal, use it; otherwise use display_review_comment (which prioritizes edited)
                final_comment = (
                    user_comment if user_comment else approval["display_review_comment"]
                )
                final_summary = approval["display_review_summary"]

                review_result = ReviewResult(
                    action=ReviewAction(approval["review_action"]),
                    comment=final_comment,
                    summary=final_summary,
                    reason=approval["review_reason"],
                    comments=inline_comments,  # Already using edited comments from approval['inline_comments']
                )

                # Post the review to GitHub
                logger.info(
                    f"Attempting to post GitHub review for PR #{pr_info.number} in {pr_info.repository_name}"
                )
                logger.info(
                    f"Review action: {review_result.action}, comment: '{review_result.comment}', inline comments: {len(review_result.comments)}"
                )
                for i, comment in enumerate(review_result.comments):
                    logger.info(
                        f"  Inline comment {i}: {comment.file}:{comment.line} - {comment.message[:100]}..."
                    )
                success = await self._post_github_review(pr_info, review_result)
                logger.info(f"GitHub review post result: {success}")

                if success:
                    # Update approval status
                    await self.database.update_pending_approval_status(
                        approval_id,
                        PENDING_APPROVAL_STATUS_APPROVED,
                        review_result.comment,
                    )

                    # Record the review in main reviews table
                    await self.database.record_review(pr_info, review_result)

                    # Play approval sound
                    if self.sound_notifier:
                        await self.sound_notifier.play_approval_sound(
                            {
                                "repo": pr_info.repository_name,
                                "pr_number": pr_info.number,
                                "author": pr_info.author,
                                "title": pr_info.title,
                            }
                        )

                    logger.info(f"Successfully approved PR {approval_id}")
                    return JSONResponse(content={"status": "success"})
                else:
                    logger.error(
                        f"Failed to post GitHub review for approval {approval_id}"
                    )
                    raise HTTPException(
                        status_code=500, detail="Failed to post GitHub review"
                    )

            except Exception as e:
                logger.error(f"Failed to approve PR {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.post("/api/approvals/{approval_id}/reject")
        async def reject_pr(approval_id: int, request: Request):
            """Reject a pending approval."""
            try:
                body = await request.json()
                reason = body.get("reason", "")

                # Update approval status
                success = await self.database.update_pending_approval_status(
                    approval_id, PENDING_APPROVAL_STATUS_REJECTED, reason
                )

                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval not found")

            except Exception as e:
                logger.error(f"Failed to reject PR {approval_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.post("/api/approvals/{approval_id}/review-again")
        async def review_again(approval_id: int, request: Request):
            """Expire pending approval and trigger fresh review."""
            try:
                if not self.llm_integration:
                    raise HTTPException(
                        status_code=500, detail="LLM integration not available"
                    )

                # Parse optional user context from request body
                user_context = None
                try:
                    body = await request.json()
                    user_context = body.get("user_context", "").strip() or None
                except Exception:
                    pass

                # Get the pending approval
                approval = await self.database.get_pending_approval(approval_id)
                if not approval:
                    raise HTTPException(status_code=404, detail="Approval not found")

                if approval["status"] != PENDING_APPROVAL_STATUS_PENDING:
                    raise HTTPException(
                        status_code=400,
                        detail="Only pending approvals can be re-reviewed",
                    )

                # Mark as expired (keeps audit trail)
                await self.database.update_pending_approval_status(
                    approval_id,
                    PENDING_APPROVAL_STATUS_EXPIRED,
                    "Re-review requested by user",
                )

                # Delete the review record to allow re-review
                await self.database.delete_review_for_re_review(
                    approval["repository"], approval["pr_number"], approval["head_sha"]
                )

                # Build PRInfo for re-review
                pr_info = PRInfo(
                    id=approval["pr_number"],
                    number=approval["pr_number"],
                    repository=approval["repository"].split("/"),
                    url=approval["pr_url"],
                    title=approval["pr_title"],
                    author=approval["pr_author"],
                    head_sha=approval["head_sha"],
                    base_sha=approval["base_sha"],
                )

                if self.llm_integration.review_in_progress:
                    logger.info(
                        "Rejecting approval re-review for %s#%s because %s is already running",
                        pr_info.repository_name,
                        pr_info.number,
                        self.llm_integration.active_review_target or "another review",
                    )
                    return self._review_busy_response()

                # Trigger fresh review in background
                re_review_context = user_context

                async def run_review():
                    try:
                        logger.info(
                            f"Starting re-review for {pr_info.repository_name}#{pr_info.number}"
                            + (f" with user context" if re_review_context else "")
                        )
                        review_result = await self.llm_integration.review_pr(
                            pr_info, user_context=re_review_context
                        )
                        logger.info(
                            f"Re-review complete for {pr_info.repository_name}#{pr_info.number}: "
                            f"{review_result.action.value}"
                        )
                        # Create new pending approval if needed
                        if review_result.action in (
                            ReviewAction.APPROVE_WITH_COMMENT,
                            ReviewAction.APPROVE_WITHOUT_COMMENT,
                        ):
                            await self.database.create_pending_approval(
                                pr_info, review_result
                            )
                            if self.sound_notifier:
                                await self.sound_notifier.play_notification(
                                    {
                                        "repo": pr_info.repository_name,
                                        "pr_number": pr_info.number,
                                        "author": pr_info.author,
                                        "title": pr_info.title,
                                    }
                                )
                        elif review_result.action == ReviewAction.REQUIRES_HUMAN_REVIEW:
                            await self.database.record_review(pr_info, review_result)
                            if self.sound_notifier:
                                await self.sound_notifier.play_human_review_sound(
                                    {
                                        "repo": pr_info.repository_name,
                                        "pr_number": pr_info.number,
                                        "author": pr_info.author,
                                        "title": pr_info.title,
                                    }
                                )
                        else:
                            # REQUEST_CHANGES or other actions
                            await self.database.record_review(pr_info, review_result)
                    except Exception as e:
                        logger.error(
                            f"Re-review failed for {pr_info.repository_name}#{pr_info.number}: {e}"
                        )
                        failure_result = ReviewResult(
                            action=ReviewAction.REQUIRES_HUMAN_REVIEW,
                            reason=f"Re-review failed: {e}",
                        )
                        await self.database.record_review(pr_info, failure_result)

                asyncio.create_task(run_review())

                logger.info(f"Triggered re-review for approval {approval_id}")
                return JSONResponse(
                    content={
                        "status": "success",
                        "message": "Review triggered. Refresh in a moment to see results.",
                    }
                )

            except HTTPException:
                raise
            except Exception as e:
                logger.error(
                    f"Failed to trigger re-review for approval {approval_id}: {e}"
                )
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.post("/api/reviews/{review_id}/review-again")
        async def review_human_review_again(review_id: int, request: Request):
            """Delete a human-review record and trigger a fresh LLM review."""
            try:
                if not self.llm_integration:
                    raise HTTPException(
                        status_code=500,
                        detail="LLM integration not available",
                    )

                # Parse optional user context from request body
                user_context = None
                try:
                    body = await request.json()
                    user_context = body.get("user_context", "").strip() or None
                except Exception:
                    pass

                review = await self.database.get_review_by_id(review_id)
                if not review:
                    raise HTTPException(status_code=404, detail="Review not found")

                if review.review_action != ReviewAction.REQUIRES_HUMAN_REVIEW:
                    raise HTTPException(
                        status_code=400,
                        detail="Only human-review records can be re-reviewed via this endpoint",
                    )

                # Delete the review record so the PR can be reviewed again
                await self.database.delete_review_for_re_review(
                    review.repository,
                    review.pr_number,
                    review.head_sha,
                )

                pr_info = PRInfo(
                    id=review.pr_number,
                    number=review.pr_number,
                    repository=review.repository.split("/"),
                    url=f"https://github.com/{review.repository}/pull/{review.pr_number}",
                    title=review.pr_title,
                    author=review.pr_author,
                    head_sha=review.head_sha,
                    base_sha=review.base_sha,
                )

                if self.llm_integration.review_in_progress:
                    logger.info(
                        "Rejecting human-review re-review for %s#%s because %s is already running",
                        pr_info.repository_name,
                        pr_info.number,
                        self.llm_integration.active_review_target or "another review",
                    )
                    return self._review_busy_response()

                re_review_context = user_context

                async def run_review():
                    try:
                        logger.info(
                            f"Starting re-review for {pr_info.repository_name}#{pr_info.number}"
                            + (f" with user context" if re_review_context else "")
                        )
                        review_result = await self.llm_integration.review_pr(
                            pr_info, user_context=re_review_context
                        )
                        logger.info(
                            f"Re-review complete for {pr_info.repository_name}#{pr_info.number}: "
                            f"{review_result.action.value}"
                        )
                        if review_result.action in (
                            ReviewAction.APPROVE_WITH_COMMENT,
                            ReviewAction.REQUEST_CHANGES,
                        ):
                            await self.database.create_pending_approval(
                                pr_info, review_result
                            )
                            if self.sound_notifier:
                                await self.sound_notifier.play_notification(
                                    {
                                        "repo": pr_info.repository_name,
                                        "pr_number": pr_info.number,
                                        "author": pr_info.author,
                                        "title": pr_info.title,
                                    }
                                )
                        elif review_result.action == ReviewAction.REQUIRES_HUMAN_REVIEW:
                            await self.database.record_review(pr_info, review_result)
                            if self.sound_notifier:
                                await self.sound_notifier.play_human_review_sound(
                                    {
                                        "repo": pr_info.repository_name,
                                        "pr_number": pr_info.number,
                                        "author": pr_info.author,
                                        "title": pr_info.title,
                                    }
                                )
                        else:
                            await self.database.record_review(pr_info, review_result)
                    except Exception as e:
                        logger.error(
                            f"Re-review failed for {pr_info.repository_name}#{pr_info.number}: {e}"
                        )
                        failure_result = ReviewResult(
                            action=ReviewAction.REQUIRES_HUMAN_REVIEW,
                            reason=f"Re-review failed: {e}",
                        )
                        await self.database.record_review(pr_info, failure_result)

                asyncio.create_task(run_review())

                logger.info(f"Triggered re-review for human review {review_id}")
                return JSONResponse(
                    content={
                        "status": "success",
                        "message": "Review triggered. Refresh in a moment to see results.",
                    }
                )

            except HTTPException:
                raise
            except Exception as e:
                logger.error(f"Failed to trigger re-review for review {review_id}: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/approved-approvals")
        async def get_approved_approvals():
            """Get approved approvals history."""
            try:
                approvals = await self.database.get_approved_approvals()
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(f"Failed to get approved approvals: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/merged-or-closed-approvals")
        async def get_merged_or_closed_approvals():
            """Get approvals cleared because PRs were merged or closed."""
            try:
                approvals = await self.database.get_pending_approvals(
                    PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED
                )
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(f"Failed to get merged/closed approvals: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/expired-approvals")
        async def get_expired_approvals():
            """Get approvals that were superseded by new commits."""
            try:
                approvals = await self.database.get_pending_approvals(
                    PENDING_APPROVAL_STATUS_EXPIRED
                )
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(f"Failed to get expired approvals: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/outdated-approvals")
        async def get_outdated_approvals_legacy():
            """Deprecated alias for merged-or-closed approvals."""
            logger.warning(
                "/api/outdated-approvals is deprecated; use /api/merged-or-closed-approvals instead"
            )
            try:
                approvals = await self.database.get_pending_approvals(
                    PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED
                )
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(
                    f"Failed to get merged/closed approvals via legacy route: {e}"
                )
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/rejected-approvals")
        async def get_rejected_approvals():
            """Get rejected approvals history."""
            try:
                approvals = await self.database.get_rejected_approvals()
                return JSONResponse(content=approvals)
            except Exception as e:
                logger.error(f"Failed to get rejected approvals: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/completed-reviews")
        async def get_completed_reviews():
            """Get completed PR reviews."""
            try:
                reviews = await self.database.get_completed_reviews()
                return JSONResponse(
                    content=[
                        {
                            "id": review.id,
                            "repository": review.repository,
                            "pr_number": review.pr_number,
                            "pr_title": review.pr_title,
                            "pr_author": review.pr_author,
                            "review_action": review.review_action.value,
                            "review_reason": review.review_reason,
                            "review_comment": review.review_comment,
                            "review_summary": review.review_summary,
                            "inline_comments_count": review.inline_comments_count,
                            "reviewed_at": review.reviewed_at,
                            "head_sha": review.head_sha,
                            "base_sha": review.base_sha,
                        }
                        for review in reviews
                    ]
                )
            except Exception as e:
                logger.error(f"Failed to get completed reviews: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/stats")
        async def get_stats():
            """Get review statistics."""
            try:
                stats = await self.database.get_review_stats()
                return JSONResponse(content=stats)
            except Exception as e:
                logger.error(f"Failed to get stats: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        # Analytics API endpoints
        @self.app.get("/api/analytics/overview")
        async def get_analytics_overview(days: Optional[int] = None):
            """Get comprehensive analytics overview."""
            try:
                overview = await self.database.get_analytics_overview(days)
                return JSONResponse(content=overview)
            except Exception as e:
                logger.error(f"Failed to get analytics overview: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/analytics/trends")
        async def get_analytics_trends(period: str = "30d"):
            """Get review trends over time."""
            try:
                if period.endswith("w"):
                    weeks = int(period[:-1])
                    data = await self.database.get_reviews_by_week(weeks)
                else:
                    days = int(period[:-1]) if period.endswith("d") else 30
                    data = await self.database.get_reviews_by_day(days)
                return JSONResponse(content=data)
            except Exception as e:
                logger.error(f"Failed to get analytics trends: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/analytics/repositories")
        async def get_analytics_repositories(
            limit: int = 20, days: Optional[int] = None
        ):
            """Get per-repository analytics."""
            try:
                data = await self.database.get_repository_stats(limit, days)
                return JSONResponse(content=data)
            except Exception as e:
                logger.error(f"Failed to get repository analytics: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/analytics/authors")
        async def get_analytics_authors(limit: int = 20, days: Optional[int] = None):
            """Get per-author analytics."""
            try:
                data = await self.database.get_author_stats(limit, days)
                return JSONResponse(content=data)
            except Exception as e:
                logger.error(f"Failed to get author analytics: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/analytics/actions")
        async def get_analytics_actions(days: Optional[int] = None):
            """Get action distribution."""
            try:
                data = await self.database.get_action_distribution(days)
                return JSONResponse(content=data)
            except Exception as e:
                logger.error(f"Failed to get action distribution: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.get("/api/analytics/pending")
        async def get_analytics_pending(days: Optional[int] = None):
            """Get pending approval statistics."""
            try:
                data = await self.database.get_pending_approval_stats(days)
                return JSONResponse(content=data)
            except Exception as e:
                logger.error(f"Failed to get pending approval stats: {e}")
                raise HTTPException(status_code=500, detail=str(e))

        # Edit/Delete API endpoints
        @self.app.post("/api/approvals/{approval_id}/update-comment")
        async def update_approval_comment(approval_id: int, request: Request):
            """Update the review comment for a pending approval."""
            try:
                body = await request.json()
                new_comment = body.get("comment", "")

                success = await self.database.update_approval_comment(
                    approval_id, new_comment
                )

                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval not found")

            except Exception as e:
                logger.error(
                    f"Failed to update comment for approval {approval_id}: {e}"
                )
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.post("/api/approvals/{approval_id}/update-summary")
        async def update_approval_summary(approval_id: int, request: Request):
            """Update the review summary for a pending approval."""
            try:
                body = await request.json()
                new_summary = body.get("summary", "")

                success = await self.database.update_approval_summary(
                    approval_id, new_summary
                )

                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval not found")

            except Exception as e:
                logger.error(
                    f"Failed to update summary for approval {approval_id}: {e}"
                )
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.post("/api/approvals/{approval_id}/update-inline-comment")
        async def update_inline_comment(approval_id: int, request: Request):
            """Update a specific inline comment for a pending approval."""
            try:
                body = await request.json()
                comment_index = body.get("index")
                new_message = body.get("message", "")

                if comment_index is None:
                    raise HTTPException(
                        status_code=400, detail="Comment index is required"
                    )

                success = await self.database.update_approval_inline_comment(
                    approval_id, comment_index, new_message
                )

                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(
                        status_code=404, detail="Approval or comment not found"
                    )

            except Exception as e:
                logger.error(
                    f"Failed to update inline comment for approval {approval_id}: {e}"
                )
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.post("/api/approvals/{approval_id}/delete-comment")
        async def delete_approval_comment(approval_id: int):
            """Delete the review comment for a pending approval."""
            try:
                success = await self.database.delete_approval_comment(approval_id)

                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval not found")

            except Exception as e:
                logger.error(
                    f"Failed to delete comment for approval {approval_id}: {e}"
                )
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.post("/api/approvals/{approval_id}/delete-summary")
        async def delete_approval_summary(approval_id: int):
            """Delete the review summary for a pending approval."""
            try:
                success = await self.database.delete_approval_summary(approval_id)

                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(status_code=404, detail="Approval not found")

            except Exception as e:
                logger.error(
                    f"Failed to delete summary for approval {approval_id}: {e}"
                )
                raise HTTPException(status_code=500, detail=str(e))

        @self.app.post("/api/approvals/{approval_id}/delete-inline-comment")
        async def delete_inline_comment(approval_id: int, request: Request):
            """Delete a specific inline comment for a pending approval."""
            try:
                body = await request.json()
                comment_index = body.get("index")

                if comment_index is None:
                    raise HTTPException(
                        status_code=400, detail="Comment index is required"
                    )

                success = await self.database.delete_approval_inline_comment(
                    approval_id, comment_index
                )

                if success:
                    return JSONResponse(content={"status": "success"})
                else:
                    raise HTTPException(
                        status_code=404, detail="Approval or comment not found"
                    )

            except Exception as e:
                logger.error(
                    f"Failed to delete inline comment for approval {approval_id}: {e}"
                )
                raise HTTPException(status_code=500, detail=str(e))

    async def _post_github_review(
        self, pr_info: PRInfo, review_result: ReviewResult
    ) -> bool:
        """Post a review to GitHub."""
        try:
            inline_comments_payload = None
            dropped_comments: List[InlineComment] = []

            if review_result.comments:
                payload, dropped = await self.github_client.prepare_inline_comments(
                    pr_info.repository,
                    pr_info.number,
                    review_result.comments,
                )
                inline_comments_payload = payload or None
                review_result.comments = [
                    InlineComment(
                        file=item["path"], line=item["line"], message=item["body"]
                    )
                    for item in payload
                ]
                dropped_comments = dropped
                logger.info(
                    "Prepared %s inline comments, dropped %s for %s/%s#%s",
                    len(payload),
                    len(dropped_comments),
                    pr_info.repository[0],
                    pr_info.repository[1],
                    pr_info.number,
                )
            else:
                review_result.comments = []

            if dropped_comments:
                fallback_text = self.github_client.format_dropped_inline_comments(
                    dropped_comments
                )
                if fallback_text:
                    if review_result.action == ReviewAction.REQUEST_CHANGES:
                        review_result.summary = self._append_text_block(
                            review_result.summary, fallback_text
                        )
                    else:
                        review_result.comment = self._append_text_block(
                            review_result.comment, fallback_text
                        )

            if review_result.action == ReviewAction.APPROVE_WITH_COMMENT:
                success = await self.github_client.approve_pr(
                    pr_info.repository,
                    pr_info.number,
                    review_result.comment,
                    inline_comments_payload,
                )
            elif review_result.action == ReviewAction.APPROVE_WITHOUT_COMMENT:
                body = review_result.comment if review_result.comment else None
                success = await self.github_client.approve_pr(
                    pr_info.repository,
                    pr_info.number,
                    body,
                    inline_comments_payload,
                )
            elif review_result.action == ReviewAction.REQUEST_CHANGES:
                success = await self.github_client.request_changes(
                    pr_info.repository,
                    pr_info.number,
                    inline_comments_payload or [],
                    review_result.summary or "Changes requested",
                )
            else:
                logger.warning(
                    f"Unsupported review action for GitHub posting: {review_result.action}"
                )
                return False

            return success

        except Exception as e:
            logger.error(f"Failed to post GitHub review: {e}")
            return False

    def get_app(self) -> FastAPI:
        """Get the FastAPI application instance."""
        return self.app
