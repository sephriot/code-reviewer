"""GitHub PR monitoring functionality."""

import asyncio
import logging
from datetime import datetime, timedelta
from typing import Optional

from .github_client import GitHubClient
from .llm_integration import LLMIntegration, LLMOutputParseError
from .config import Config
from .sound_notifier import SoundNotifier
from .database import (
    ReviewDatabase,
    PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED,
    OWN_PR_STATUS_PENDING,
    OWN_PR_STATUS_READY_FOR_MERGING,
    OWN_PR_STATUS_NEEDS_ATTENTION,
    OWN_PR_STATUS_MERGED,
    OWN_PR_STATUS_CLOSED,
    OWN_PR_STATUS_EXPIRED,
)
from .models import PRInfo, ReviewResult, ReviewAction


logger = logging.getLogger(__name__)


class GitHubMonitor:
    def __init__(
        self,
        github_client: GitHubClient,
        model_integration: LLMIntegration,
        config: Config,
    ):
        self.github_client = github_client
        self.llm_integration = model_integration
        self.config = config
        self.running = True
        self.sound_notifier = SoundNotifier(
            enabled=config.sound_enabled,
            sound_file=config.sound_file,
            approval_sound_enabled=config.approval_sound_enabled,
            approval_sound_file=config.approval_sound_file,
            timeout_sound_enabled=config.timeout_sound_enabled,
            timeout_sound_file=config.timeout_sound_file,
            merged_or_closed_sound_enabled=config.merged_or_closed_sound_enabled,
            merged_or_closed_sound_file=config.merged_or_closed_sound_file,
            own_pr_ready_sound_enabled=config.own_pr_ready_sound_enabled,
            own_pr_ready_sound_file=config.own_pr_ready_sound_file,
            own_pr_needs_attention_sound_enabled=config.own_pr_needs_attention_sound_enabled,
            own_pr_needs_attention_sound_file=config.own_pr_needs_attention_sound_file,
            review_started_sound_enabled=config.review_started_sound_enabled,
            review_started_sound_file=config.review_started_sound_file,
        )
        self.db = ReviewDatabase(config.database_path)

    async def start_monitoring(self):
        """Start monitoring for new PRs."""
        mode = "DRY RUN" if self.config.dry_run else "LIVE"
        logger.info(f"Starting GitHub PR monitoring in {mode} mode...")

        if self.config.dry_run:
            logger.info(
                "DRY RUN MODE: No actual GitHub actions will be performed, only logged"
            )

        if self.config.repositories:
            logger.info(
                f"Repository filtering enabled: {', '.join(self.config.repositories)}"
            )
        else:
            logger.info("Monitoring all repositories where you have review access")

        if self.config.pr_authors:
            logger.info(
                f"PR author filtering enabled: {', '.join(self.config.pr_authors)}"
            )
        else:
            logger.info("Monitoring PRs from all authors")

        while self.running:
            try:
                await self._check_for_new_prs()
                # Use asyncio.sleep with shorter intervals to allow faster shutdown
                for _ in range(self.config.poll_interval):
                    if not self.running:
                        break
                    await asyncio.sleep(1)
            except Exception as e:
                logger.error(f"Error during PR monitoring: {e}")
                # Sleep with interruption check
                for _ in range(self.config.poll_interval):
                    if not self.running:
                        break
                    await asyncio.sleep(1)

        logger.info("PR monitoring stopped")

    def cleanup_sync(self):
        """Cleanup resources synchronously."""
        logger.info("Cleaning up GitHub monitor resources...")
        self.running = False

        # Close GitHub client session if it exists
        if hasattr(self.github_client, "session") and self.github_client.session:
            if not self.github_client.session.closed:
                logger.debug("Closing aiohttp session...")
                import asyncio

                # Just mark the session for closure - the event loop will handle it
                logger.debug("Marking aiohttp session for garbage collection")
                self.github_client.session = None

        # Database cleanup (sync)
        try:
            if hasattr(self.db, "close"):
                self.db.close()
                logger.debug("Database closed successfully")
        except Exception as e:
            logger.error(f"Error closing database: {e}")

        logger.info("Cleanup completed")

    async def _check_for_new_prs(self):
        """Check for new PRs where user is assigned as reviewer."""
        logger.debug("Checking for new PRs to review...")
        try:
            try:
                await self._expire_merged_or_closed_pending_approvals()
            except Exception as cleanup_error:
                logger.error(
                    f"Error while expiring merged/closed approvals: {cleanup_error}"
                )

            prs = await self.github_client.get_review_requests(
                self.config.github_username,
                self.config.repositories,
                self.config.pr_authors,
            )

            if prs:
                logger.info(f"Found {len(prs)} PR(s) pending review")
            else:
                logger.debug("No PRs found pending review")

            for pr_info in prs:
                repo_name = pr_info.repository_name
                logger.debug(
                    f"Checking PR #{pr_info.number} in {repo_name} (head: {pr_info.head_sha[:8] if pr_info.head_sha else 'unknown'})"
                )

                # Check if we should review this PR based on commit SHA comparison
                should_review = await self.db.should_review_pr(pr_info)

                if should_review:
                    logger.info(f"Found PR to review: #{pr_info.number} in {repo_name}")
                    # Play notification sound for new PR discovery in dry run mode only
                    if self.config.dry_run:
                        logger.debug(
                            "Playing notification sound for new PR discovery (dry run mode)"
                        )
                        await self.sound_notifier.play_notification()

                    await self._process_pr(pr_info)
                else:
                    # Get more specific reason for skipping
                    await self._log_skip_reason(pr_info)

        except Exception as e:
            logger.error(f"Error checking for PRs: {e}")

    async def _process_pr(self, pr_info: PRInfo) -> None:
        """Process a single PR for review."""
        repo_name = pr_info.repository_name
        logger.info(f"Processing PR #{pr_info.number} in {repo_name}")
        try:
            previous_pending_context = None
            previous_pending = await self.db.get_latest_pending_approval_for_pr(
                pr_info.repository_name,
                pr_info.number,
            )

            if (
                previous_pending
                and previous_pending.get("head_sha") != pr_info.head_sha
            ):
                previous_pending_context = previous_pending
                expired_ids = await self.db.expire_pending_approvals_for_pr(pr_info)
                if expired_ids:
                    logger.info(
                        "Expired pending approval IDs %s for PR #%s before reviewing new commit %s",
                        expired_ids,
                        pr_info.number,
                        pr_info.head_sha[:8] if pr_info.head_sha else "unknown",
                    )

            # Post 👀 comment and play sound before review starts
            if self.config.review_started_comment_enabled:
                if self.config.dry_run:
                    logger.info(
                        f"[DRY RUN] Would post '👀' comment on PR #{pr_info.number}"
                    )
                else:
                    logger.debug(f"Posting '👀' comment on PR #{pr_info.number}")
                    await self.github_client.add_review_comment(
                        pr_info.repository, pr_info.number, "👀"
                    )

            if self.config.dry_run:
                logger.info(
                    f"[DRY RUN] Would play review started sound for PR #{pr_info.number}"
                )
            else:
                logger.debug(f"Playing review started sound for PR #{pr_info.number}")
                await self.sound_notifier.play_review_started_sound(
                    {
                        "repo": pr_info.repository_name,
                        "pr_number": pr_info.number,
                        "author": pr_info.author,
                        "title": pr_info.title,
                    }
                )

            # Run model-driven code review - the CLI will fetch all PR details
            logger.debug(
                f"Running {self.config.review_model.value} code review for PR #{pr_info.number}"
            )
            review_result = await self.llm_integration.review_pr(
                pr_info,
                timeout=self.config.review_timeout
                if self.config.review_timeout
                else None,
                previous_pending=previous_pending_context,
            )

            # Log the review output
            await self._log_review_output(pr_info, review_result)

            # Act on the review result
            await self._act_on_review(pr_info, review_result)

            # Record the review in database (unless in dry run mode or pending approval)
            if not self.config.dry_run and review_result.action not in [
                ReviewAction.APPROVE_WITH_COMMENT,
                ReviewAction.REQUEST_CHANGES,
            ]:
                await self.db.record_review(pr_info, review_result)
            elif self.config.dry_run:
                logger.info(
                    f"[DRY RUN] Would record review in database for PR #{pr_info.number}"
                )
            # Note: APPROVE_WITH_COMMENT and REQUEST_CHANGES reviews are recorded when the human approves via web UI

        except asyncio.TimeoutError:
            timeout_seconds = self.config.review_timeout
            logger.error(
                f"⚠️ Review timed out after {timeout_seconds} seconds for PR #{pr_info.number} in {repo_name}"
            )
            reason = f"Automated review timed out after {timeout_seconds} seconds"
            if self.config.dry_run:
                logger.info(
                    f"[DRY RUN] Would mark PR #{pr_info.number} as REQUIRES HUMAN REVIEW due to timeout"
                )
                logger.info("[DRY RUN] Would play timeout sound notification")
            else:
                timeout_result = ReviewResult(
                    action=ReviewAction.REQUIRES_HUMAN_REVIEW,
                    reason=reason,
                )
                await self.db.record_review(pr_info, timeout_result)
                await self.sound_notifier.play_timeout_sound(
                    {
                        "repo": pr_info.repository_name,
                        "pr_number": pr_info.number,
                        "author": pr_info.author,
                        "title": pr_info.title,
                    }
                )
            return

        except LLMOutputParseError as e:
            logger.error(
                f"❌ Review failed for PR #{pr_info.number} in {repo_name}: Invalid JSON output from {self.config.review_model.value}"
            )
            logger.error(f"📋 PR: '{pr_info.title}' by {pr_info.author}")
            logger.error(f"❗ Reason: {str(e)}")
            # Log a preview of the output (truncated for readability)
            output_preview = (
                e.raw_output[:1000] + "..."
                if len(e.raw_output) > 1000
                else e.raw_output
            )
            logger.error(
                f"📤 {self.config.review_model.value} output preview: {output_preview}"
            )
            reason = f"Automated review failed: invalid JSON output from {self.config.review_model.value} — {e}"
            if self.config.dry_run:
                logger.info(
                    f"[DRY RUN] Would mark PR #{pr_info.number} as REQUIRES HUMAN REVIEW due to parse error"
                )
            else:
                failure_result = ReviewResult(
                    action=ReviewAction.REQUIRES_HUMAN_REVIEW,
                    reason=reason,
                )
                await self.db.record_review(pr_info, failure_result)
                await self.sound_notifier.play_notification()

        except Exception as e:
            logger.error(f"Error processing PR #{pr_info.number}: {e}")
            reason = f"Automated review failed with unexpected error: {e}"
            if self.config.dry_run:
                logger.info(
                    f"[DRY RUN] Would mark PR #{pr_info.number} as REQUIRES HUMAN REVIEW due to error"
                )
            else:
                failure_result = ReviewResult(
                    action=ReviewAction.REQUIRES_HUMAN_REVIEW,
                    reason=reason,
                )
                await self.db.record_review(pr_info, failure_result)
                await self.sound_notifier.play_notification()

    async def _expire_merged_or_closed_pending_approvals(self) -> None:
        """Mark pending approvals as merged_or_closed if their PRs are merged or closed."""
        pending_refs = await self.db.get_pending_approval_refs()

        if not pending_refs:
            return

        logger.debug(
            f"Checking {len(pending_refs)} pending approval(s) for merged/closed status"
        )

        status_tasks = [
            self.github_client.get_pr_status(ref["repository"], ref["pr_number"])
            for ref in pending_refs
        ]

        results = await asyncio.gather(*status_tasks, return_exceptions=True)

        for ref, result in zip(pending_refs, results):
            if isinstance(result, Exception):
                logger.error(
                    f"Failed to fetch PR status for {ref['repository']}#{ref['pr_number']}: {result}"
                )
                continue

            if not result:
                logger.warning(
                    f"Unable to determine PR status for {ref['repository']}#{ref['pr_number']}"
                )
                continue

            state = result.get("state")
            merged = bool(result.get("merged"))

            if state != "open" or merged:
                reason = "merged" if merged else state or "unknown"
                updated = await self.db.update_pending_approval_status(
                    ref["id"], PENDING_APPROVAL_STATUS_MERGED_OR_CLOSED
                )
                if updated:
                    logger.info(
                        f"Marked pending approval ID {ref['id']} for PR #{ref['pr_number']} in "
                        f"{ref['repository']} as MERGED_OR_CLOSED ({reason})"
                    )
                    await self.sound_notifier.play_merged_or_closed_sound(
                        {
                            "repo": ref["repository"],
                            "pr_number": ref["pr_number"],
                            "author": ref.get("author", ""),
                            "title": ref.get("title", ""),
                        }
                    )
                else:
                    logger.debug(
                        f"Pending approval ID {ref['id']} already processed when marking as MERGED_OR_CLOSED"
                    )

    async def _act_on_review(self, pr_info: PRInfo, review_result: ReviewResult):
        """Act on the model's review result."""
        action = review_result.action

        if self.config.dry_run:
            await self._log_dry_run_action(pr_info, action, review_result)
            return

        if action == ReviewAction.APPROVE_WITH_COMMENT:
            # Create pending approval instead of immediate approval
            await self.db.create_pending_approval(pr_info, review_result)
            logger.info(
                f"Created pending approval for PR #{pr_info.number} - awaiting human confirmation"
            )

            # Play notification sound for human attention
            await self.sound_notifier.play_notification()

        elif action == ReviewAction.APPROVE_WITHOUT_COMMENT:
            await self.github_client.approve_pr(pr_info.repository, pr_info.number)
            logger.info(f"Approved PR #{pr_info.number} without comment")

            # Play approval sound
            await self.sound_notifier.play_approval_sound(
                {
                    "repo": pr_info.repository_name,
                    "pr_number": pr_info.number,
                    "author": pr_info.author,
                    "title": pr_info.title,
                }
            )

        elif action == ReviewAction.REQUEST_CHANGES:
            # Create pending approval instead of immediate change request
            await self.db.create_pending_approval(pr_info, review_result)
            logger.info(
                f"Created pending change request for PR #{pr_info.number} - awaiting human confirmation"
            )

            # Play notification sound for human attention
            await self.sound_notifier.play_notification()

        elif action == ReviewAction.REQUIRES_HUMAN_REVIEW:
            reason = review_result.reason or "PR requires human review"
            logger.info(f"PR #{pr_info.number} requires human review: {reason}")

            # Play notification sound
            await self.sound_notifier.play_notification()

    async def _log_skip_reason(self, pr_info: PRInfo):
        """Log the specific reason why a PR is being skipped."""
        repository = pr_info.repository_name
        pr_number = pr_info.number

        # Get the latest review to determine the specific skip reason
        latest_review = await self.db.get_latest_review(repository, pr_number)

        if not latest_review:
            # This shouldn't happen if should_review_pr returned False, but handle it
            logger.info(
                f"Skipping PR #{pr_number} in {repository} - no previous review found but marked to skip"
            )
            return

        action = latest_review.review_action

        if action in [
            ReviewAction.APPROVE_WITH_COMMENT,
            ReviewAction.APPROVE_WITHOUT_COMMENT,
            ReviewAction.REQUEST_CHANGES,
        ]:
            logger.info(
                f"Skipping PR #{pr_number} in {repository} - already reviewed with action '{action.value}'"
            )
        elif action == ReviewAction.REQUIRES_HUMAN_REVIEW:
            # This case should not occur since should_review_pr returns True for human review
            # But keeping it for completeness
            logger.info(
                f"Skipping PR #{pr_number} in {repository} - marked for human review"
            )
        else:
            logger.info(
                f"Skipping PR #{pr_number} in {repository} - previous action: {action.value}"
            )

    async def _log_review_output(self, pr_info: PRInfo, review_result: ReviewResult):
        """Log the complete review output for visibility."""
        repo_name = pr_info.repository_name
        pr_number = pr_info.number
        action = review_result.action

        logger.info(f"🔍 Review completed for PR #{pr_number} in {repo_name}")
        logger.info(f"📋 PR: '{pr_info.title}' by {pr_info.author}")
        logger.info(f"⚡ Action: {action.value.upper()}")

        # Log reason if provided
        if review_result.reason:
            logger.info(f"💭 Reason: {review_result.reason}")

        # Log comment if provided
        if review_result.comment:
            logger.info(f"💬 Comment: {review_result.comment}")

        # Log summary if provided
        if review_result.summary:
            logger.info(f"📝 Summary: {review_result.summary}")

        # Log inline comments if any
        if review_result.comments:
            logger.info(f"📍 Inline comments ({len(review_result.comments)}):")
            for i, comment in enumerate(review_result.comments, 1):
                logger.info(
                    f"   {i}. {comment.file}:{comment.line} - {comment.message}"
                )

    async def _log_dry_run_action(
        self, pr_info: PRInfo, action: ReviewAction, review_result: ReviewResult
    ):
        """Log what would be done in dry run mode."""
        repo_name = pr_info.repository_name
        pr_number = pr_info.number

        logger.info(f"[DRY RUN] PR #{pr_number} in {repo_name}")

        if action == ReviewAction.APPROVE_WITH_COMMENT:
            comment = review_result.comment or ""
            logger.info(
                f"[DRY RUN] Would APPROVE PR #{pr_number} with comment: {comment}"
            )

        elif action == ReviewAction.APPROVE_WITHOUT_COMMENT:
            logger.info(f"[DRY RUN] Would APPROVE PR #{pr_number} without comment")

        elif action == ReviewAction.REQUEST_CHANGES:
            summary = (
                review_result.summary or "Changes requested based on automated review"
            )
            comments = review_result.comments
            logger.info(f"[DRY RUN] Would REQUEST CHANGES for PR #{pr_number}")
            logger.info(f"[DRY RUN] Summary: {summary}")
            if comments:
                logger.info(f"[DRY RUN] Would add {len(comments)} inline comments:")
                for i, comment in enumerate(comments[:3], 1):  # Log first 3 comments
                    logger.info(
                        f"[DRY RUN]   {i}. {comment.file}:{comment.line} - {comment.message}"
                    )
                if len(comments) > 3:
                    logger.info(
                        f"[DRY RUN]   ... and {len(comments) - 3} more comments"
                    )

        elif action == ReviewAction.REQUIRES_HUMAN_REVIEW:
            reason = review_result.reason or "PR requires human review"
            logger.info(
                f"[DRY RUN] Would mark PR #{pr_number} as REQUIRING HUMAN REVIEW"
            )
            logger.info(f"[DRY RUN] Reason: {reason}")
            logger.info(f"[DRY RUN] Would play notification sound (if enabled)")

    def stop_monitoring(self):
        """Stop the monitoring loop."""
        self.running = False

    def cleanup(self):
        """Cleanup resources (async version)."""
        self.running = False

    async def start_own_prs_monitoring(self):
        """Start monitoring own PRs for review."""
        if not self.config.own_pr_enabled:
            logger.info("Own PR monitoring is disabled")
            return

        mode = "DRY RUN" if self.config.dry_run else "LIVE"
        logger.info(f"Starting own PR monitoring in {mode} mode...")

        if self.config.repositories:
            logger.info(
                f"Monitoring own PRs in repositories: {', '.join(self.config.repositories)}"
            )
        else:
            logger.warning(
                "No repositories specified for own PR monitoring - please configure REPOSITORIES"
            )

        while self.running:
            try:
                await self._check_for_own_prs()
                for _ in range(self.config.poll_interval):
                    if not self.running:
                        break
                    await asyncio.sleep(1)
            except Exception as e:
                logger.error(f"Error during own PR monitoring: {e}")
                for _ in range(self.config.poll_interval):
                    if not self.running:
                        break
                    await asyncio.sleep(1)

        logger.info("Own PR monitoring stopped")

    async def _check_for_own_prs(self):
        """Check for own PRs that need review."""
        logger.debug("Checking for own PRs to review...")

        await self._expire_merged_or_closed_own_prs()

        prs = await self.github_client.get_own_prs(
            self.config.github_username, self.config.repositories
        )

        if prs:
            logger.info(f"Found {len(prs)} own PR(s)")
        else:
            logger.debug("No own PRs found")

        for pr_info in prs:
            repo_name = pr_info.repository_name
            logger.debug(
                f"Checking own PR #{pr_info.number} in {repo_name} (head: {pr_info.head_sha[:8] if pr_info.head_sha else 'unknown'})"
            )

            existing_pr = await self.db.get_own_pr_by_pr_number(
                repo_name, pr_info.number
            )

            if existing_pr and existing_pr.get("head_sha") == pr_info.head_sha:
                logger.debug(
                    f"Own PR #{pr_info.number} already tracked with same head SHA, skipping"
                )
                continue

            if existing_pr:
                logger.info(
                    f"New commit detected for own PR #{pr_info.number}, expiring old reviews..."
                )
                await self.db.expire_own_prs_for_pr(repo_name, pr_info.number)

            logger.info(f"Found own PR to review: #{pr_info.number} in {repo_name}")
            await self._process_own_pr(pr_info)

    async def _process_own_pr(self, pr_info: PRInfo):
        """Process a single own PR for review."""
        repo_name = pr_info.repository_name
        logger.info(f"Processing own PR #{pr_info.number} in {repo_name}")

        try:
            if self.config.dry_run:
                logger.info(
                    f"[DRY RUN] Would play review started sound for own PR #{pr_info.number}"
                )
            else:
                logger.debug(
                    f"Playing review started sound for own PR #{pr_info.number}"
                )
                await self.sound_notifier.play_review_started_sound(
                    {
                        "repo": pr_info.repository_name,
                        "pr_number": pr_info.number,
                        "author": pr_info.author,
                        "title": pr_info.title,
                    }
                )

            logger.debug(
                f"Running {self.config.review_model.value} code review for own PR #{pr_info.number}"
            )
            review_result = await self.llm_integration.review_pr(
                pr_info,
                timeout=self.config.review_timeout
                if self.config.review_timeout
                else None,
            )

            await self._log_review_output(pr_info, review_result)

            if review_result.action in (
                ReviewAction.APPROVE_WITHOUT_COMMENT,
                ReviewAction.APPROVE_WITH_COMMENT,
            ):
                status = OWN_PR_STATUS_READY_FOR_MERGING
                await self.db.create_own_pr(pr_info, review_result)
                logger.info(f"Own PR #{pr_info.number} is ready for merging")
                if not self.config.dry_run:
                    await self.sound_notifier.play_pr_ready_sound(
                        {
                            "repo": pr_info.repository_name,
                            "pr_number": pr_info.number,
                            "author": pr_info.author,
                            "title": pr_info.title,
                        }
                    )
            else:
                status = OWN_PR_STATUS_NEEDS_ATTENTION
                await self.db.create_own_pr(pr_info, review_result)
                logger.info(f"Own PR #{pr_info.number} needs attention")
                if not self.config.dry_run:
                    await self.sound_notifier.play_pr_needs_attention_sound(
                        {
                            "repo": pr_info.repository_name,
                            "pr_number": pr_info.number,
                            "author": pr_info.author,
                            "title": pr_info.title,
                        }
                    )

        except asyncio.TimeoutError:
            timeout_seconds = self.config.review_timeout
            logger.error(
                f"Own PR review timed out after {timeout_seconds} seconds for PR #{pr_info.number}"
            )
            if not self.config.dry_run:
                await self.db.create_own_pr(
                    pr_info,
                    ReviewResult(
                        action=ReviewAction.REQUIRES_HUMAN_REVIEW,
                        reason=f"Review timed out after {timeout_seconds} seconds",
                    ),
                )
                await self.sound_notifier.play_pr_needs_attention_sound(
                    {
                        "repo": pr_info.repository_name,
                        "pr_number": pr_info.number,
                        "author": pr_info.author,
                        "title": pr_info.title,
                    }
                )

        except LLMOutputParseError as e:
            logger.error(
                f"Own PR review failed for PR #{pr_info.number}: Invalid JSON output"
            )
            if not self.config.dry_run:
                await self.db.create_own_pr(
                    pr_info,
                    ReviewResult(
                        action=ReviewAction.REQUIRES_HUMAN_REVIEW,
                        reason=f"Review failed: invalid JSON output",
                    ),
                )
                await self.sound_notifier.play_pr_needs_attention_sound(
                    {
                        "repo": pr_info.repository_name,
                        "pr_number": pr_info.number,
                        "author": pr_info.author,
                        "title": pr_info.title,
                    }
                )

        except Exception as e:
            logger.error(f"Error processing own PR #{pr_info.number}: {e}")
            if not self.config.dry_run:
                await self.db.create_own_pr(
                    pr_info,
                    ReviewResult(
                        action=ReviewAction.REQUIRES_HUMAN_REVIEW,
                        reason=f"Review failed with error: {e}",
                    ),
                )
                await self.sound_notifier.play_pr_needs_attention_sound(
                    {
                        "repo": pr_info.repository_name,
                        "pr_number": pr_info.number,
                        "author": pr_info.author,
                        "title": pr_info.title,
                    }
                )

    async def _expire_merged_or_closed_own_prs(self):
        """Mark own PRs as merged or closed if their PRs are merged or closed."""
        own_prs = await self.db.get_own_prs()

        active_prs = [
            pr
            for pr in own_prs
            if pr["status"]
            in (
                OWN_PR_STATUS_PENDING,
                OWN_PR_STATUS_READY_FOR_MERGING,
                OWN_PR_STATUS_NEEDS_ATTENTION,
            )
        ]

        if not active_prs:
            return

        logger.debug(f"Checking {len(active_prs)} own PR(s) for merged/closed status")

        status_tasks = [
            self.github_client.get_pr_status(pr["repository"], pr["pr_number"])
            for pr in active_prs
        ]

        results = await asyncio.gather(*status_tasks, return_exceptions=True)

        for pr, result in zip(active_prs, results):
            if isinstance(result, Exception):
                logger.error(
                    f"Failed to fetch PR status for {pr['repository']}#{pr['pr_number']}: {result}"
                )
                continue

            if not result:
                continue

            state = result.get("state")
            merged = bool(result.get("merged"))

            if state != "open" or merged:
                new_status = OWN_PR_STATUS_MERGED if merged else OWN_PR_STATUS_CLOSED
                await self.db.update_own_pr_status(pr["id"], new_status)
                logger.info(
                    f"Marked own PR #{pr['pr_number']} in {pr['repository']} as {new_status}"
                )
        """Clean up resources."""
        if hasattr(self, "db"):
            self.db.close()
