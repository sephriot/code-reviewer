"""GitHub PR monitoring functionality."""

import asyncio
import logging
from datetime import datetime, timedelta
from typing import Set, Optional

from .github_client import GitHubClient
from .claude_integration import ClaudeIntegration, ReviewAction
from .config import Config
from .sound_notifier import SoundNotifier
from .database import ReviewDatabase


logger = logging.getLogger(__name__)


class GitHubMonitor:
    def __init__(self, github_client: GitHubClient, claude_integration: ClaudeIntegration, config: Config):
        self.github_client = github_client
        self.claude_integration = claude_integration
        self.config = config
        self.processed_prs: Set[int] = set()
        self.running = True
        self.sound_notifier = SoundNotifier(
            enabled=config.sound_enabled,
            sound_file=config.sound_file
        )
        self.db = ReviewDatabase(config.database_path)
        
    async def start_monitoring(self):
        """Start monitoring for new PRs."""
        mode = "DRY RUN" if self.config.dry_run else "LIVE"
        logger.info(f"Starting GitHub PR monitoring in {mode} mode...")
        
        # Play startup notification sound
        logger.debug("Playing startup notification sound")
        await self.sound_notifier.play_notification()
        
        if self.config.dry_run:
            logger.info("DRY RUN MODE: No actual GitHub actions will be performed, only logged")
            
        if self.config.repositories:
            logger.info(f"Repository filtering enabled: {', '.join(self.config.repositories)}")
        else:
            logger.info("Monitoring all repositories where you have review access")
        
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
        if hasattr(self.github_client, 'session') and self.github_client.session:
            if not self.github_client.session.closed:
                logger.debug("Closing aiohttp session...")
                import asyncio
                # Just mark the session for closure - the event loop will handle it
                logger.debug("Marking aiohttp session for garbage collection")
                self.github_client.session = None
        
        # Database cleanup (sync)
        try:
            if hasattr(self.db, 'close'):
                self.db.close()
                logger.debug("Database closed successfully")
        except Exception as e:
            logger.error(f"Error closing database: {e}")
            
        logger.info("Cleanup completed")
        
    async def _check_for_new_prs(self):
        """Check for new PRs where user is assigned as reviewer."""
        logger.debug("Checking for new PRs to review...")
        try:
            prs = await self.github_client.get_review_requests(
                self.config.github_username, 
                self.config.repositories
            )
            
            if prs:
                logger.info(f"Found {len(prs)} PR(s) pending review")
            else:
                logger.debug("No PRs found pending review")
            
            for pr_info in prs:
                pr_id = pr_info['id']
                
                if pr_id not in self.processed_prs:
                    repo_name = '/'.join(pr_info['repository'])
                    logger.info(f"Found new PR to review: #{pr_info['number']} in {repo_name}")
                    # Play notification sound for new PR discovery in dry run mode only
                    if self.config.dry_run:
                        logger.debug("Playing notification sound for new PR discovery (dry run mode)")
                        await self.sound_notifier.play_notification()
                    
                    # Check if we should review this PR based on history (simplified check)
                    should_review = await self.db.should_review_pr(pr_info)
                    
                    if should_review:
                        await self._process_pr(pr_info)
                    else:
                        logger.info(f"Skipping PR #{pr_info['number']} - already reviewed or requires human attention")
                    
                    self.processed_prs.add(pr_id)
                    
        except Exception as e:
            logger.error(f"Error checking for PRs: {e}")
            
    async def _process_pr(self, pr_info: dict):
        """Process a single PR for review."""
        repo_name = '/'.join(pr_info['repository'])
        logger.info(f"Processing PR #{pr_info['number']} in {repo_name}")
        try:
            # Run Claude code review - Claude Code will fetch all PR details
            logger.debug(f"Running Claude code review for PR #{pr_info['number']}")
            review_result = await self.claude_integration.review_pr(pr_info)
            
            # Act on the review result
            await self._act_on_review(pr_info, review_result)
            
            # Record the review in database (unless in dry run mode)
            if not self.config.dry_run:
                await self.db.record_review(pr_info, review_result)
            else:
                logger.info(f"[DRY RUN] Would record review in database for PR #{pr_info['number']}")
            
        except Exception as e:
            logger.error(f"Error processing PR #{pr_info['number']}: {e}")
            
    async def _act_on_review(self, pr_info: dict, review_result: dict):
        """Act on Claude's review result."""
        action = ReviewAction(review_result['action'])
        
        if self.config.dry_run:
            await self._log_dry_run_action(pr_info, action, review_result)
            return
        
        if action == ReviewAction.APPROVE_WITH_COMMENT:
            await self.github_client.approve_pr(
                pr_info['repository'],
                pr_info['number'],
                review_result.get('comment', '')
            )
            logger.info(f"Approved PR #{pr_info['number']} with comment")
            
        elif action == ReviewAction.APPROVE_WITHOUT_COMMENT:
            await self.github_client.approve_pr(
                pr_info['repository'],
                pr_info['number']
            )
            logger.info(f"Approved PR #{pr_info['number']} without comment")
            
        elif action == ReviewAction.REQUEST_CHANGES:
            await self.github_client.request_changes(
                pr_info['repository'],
                pr_info['number'],
                review_result.get('comments', []),
                review_result.get('summary', 'Changes requested based on automated review')
            )
            logger.info(f"Requested changes for PR #{pr_info['number']}")
            
        elif action == ReviewAction.REQUIRES_HUMAN_REVIEW:
            reason = review_result.get('reason', 'PR requires human review')
            logger.info(f"PR #{pr_info['number']} requires human review: {reason}")
            
            # Play notification sound
            await self.sound_notifier.play_notification()
            
    async def _log_dry_run_action(self, pr_info: dict, action: ReviewAction, review_result: dict):
        """Log what would be done in dry run mode."""
        repo_name = '/'.join(pr_info['repository'])
        pr_number = pr_info['number']
        pr_title = pr_info['title']
        
        logger.info(f"[DRY RUN] PR #{pr_number} in {repo_name}: '{pr_title}'")
        
        if action == ReviewAction.APPROVE_WITH_COMMENT:
            comment = review_result.get('comment', '')
            logger.info(f"[DRY RUN] Would APPROVE PR #{pr_number} with comment: {comment}")
            
        elif action == ReviewAction.APPROVE_WITHOUT_COMMENT:
            logger.info(f"[DRY RUN] Would APPROVE PR #{pr_number} without comment")
            
        elif action == ReviewAction.REQUEST_CHANGES:
            summary = review_result.get('summary', 'Changes requested based on automated review')
            comments = review_result.get('comments', [])
            logger.info(f"[DRY RUN] Would REQUEST CHANGES for PR #{pr_number}")
            logger.info(f"[DRY RUN] Summary: {summary}")
            if comments:
                logger.info(f"[DRY RUN] Would add {len(comments)} inline comments:")
                for i, comment in enumerate(comments[:3], 1):  # Log first 3 comments
                    logger.info(f"[DRY RUN]   {i}. {comment.get('file', 'unknown')}:{comment.get('line', '?')} - {comment.get('message', '')}")
                if len(comments) > 3:
                    logger.info(f"[DRY RUN]   ... and {len(comments) - 3} more comments")
                    
        elif action == ReviewAction.REQUIRES_HUMAN_REVIEW:
            reason = review_result.get('reason', 'PR requires human review')
            logger.info(f"[DRY RUN] Would mark PR #{pr_number} as REQUIRING HUMAN REVIEW")
            logger.info(f"[DRY RUN] Reason: {reason}")
            logger.info(f"[DRY RUN] Would play notification sound (if enabled)")
            
    def stop_monitoring(self):
        """Stop the monitoring loop."""
        self.running = False
        
    def cleanup(self):
        """Clean up resources."""
        if hasattr(self, 'db'):
            self.db.close()