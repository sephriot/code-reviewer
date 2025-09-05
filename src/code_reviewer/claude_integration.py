"""Claude Code integration for PR reviews."""

import asyncio
import json
import logging
import subprocess
import tempfile
from enum import Enum
from pathlib import Path
from typing import Dict, Any, Optional


logger = logging.getLogger(__name__)


class ReviewAction(Enum):
    APPROVE_WITH_COMMENT = "approve_with_comment"
    APPROVE_WITHOUT_COMMENT = "approve_without_comment"
    REQUEST_CHANGES = "request_changes"
    REQUIRES_HUMAN_REVIEW = "requires_human_review"


class ClaudeIntegration:
    def __init__(self, prompt_file: Path):
        self.prompt_file = prompt_file
        
    async def review_pr(self, pr_info: dict) -> dict:
        """Review PR using Claude Code - Claude Code will fetch all PR information."""
        try:
            # Run Claude Code with the PR URL - Claude Code will handle all data fetching
            result = await self._run_claude_code(pr_info)
            
            # Parse and validate result
            return self._parse_review_result(result)
                
        except Exception as e:
            logger.error(f"Error during Claude review: {e}")
            raise
            
                
    async def _run_claude_code(self, pr_info: dict) -> str:
        """Run Claude Code with the review prompt and PR URL."""
        try:
            # Read the prompt template
            prompt_template = self.prompt_file.read_text(encoding='utf-8')
            
            # Build the full prompt with PR URL
            pr_url = pr_info['url']
            repo_name = '/'.join(pr_info['repository'])
            pr_number = pr_info['number']
            
            full_prompt = f"""Please review this GitHub pull request: {pr_url}

Repository: {repo_name}
PR Number: #{pr_number}

{prompt_template}

Please analyze the pull request at {pr_url} and provide your review following the instructions above."""

            # Run Claude Code directly with the prompt
            cmd = ['claude', '--print']
            
            process = await asyncio.create_subprocess_exec(
                *cmd,
                input=full_prompt.encode(),
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE
            )
            
            stdout, stderr = await process.communicate()
            
            if process.returncode != 0:
                raise RuntimeError(f"Claude Code failed: {stderr.decode()}")
                
            return stdout.decode()
            
        except Exception as e:
            logger.error(f"Error running Claude Code: {e}")
            raise
            
            
    def _parse_review_result(self, claude_output: str) -> dict:
        """Parse Claude's review output into structured result."""
        try:
            # Look for JSON output in Claude's response
            lines = claude_output.strip().split('\n')
            
            for line in lines:
                line = line.strip()
                if line.startswith('{') and line.endswith('}'):
                    try:
                        result = json.loads(line)
                        self._validate_review_result(result)
                        return result
                    except json.JSONDecodeError:
                        continue
                        
            # If no JSON found, try to parse structured text
            return self._parse_text_result(claude_output)
            
        except Exception as e:
            logger.error(f"Error parsing Claude output: {e}")
            # Return default safe action
            return {
                'action': ReviewAction.REQUEST_CHANGES.value,
                'summary': 'Failed to parse review result',
                'comments': []
            }
            
    def _validate_review_result(self, result: dict):
        """Validate the review result structure."""
        required_fields = ['action']
        
        for field in required_fields:
            if field not in result:
                raise ValueError(f"Missing required field: {field}")
                
        # Validate action value
        try:
            ReviewAction(result['action'])
        except ValueError:
            raise ValueError(f"Invalid action: {result['action']}")
            
    def _parse_text_result(self, output: str) -> dict:
        """Parse text-based review result as fallback."""
        # Simple text parsing - this could be enhanced based on your prompt format
        output_lower = output.lower()
        
        if 'human' in output_lower and 'review' in output_lower:
            return {
                'action': ReviewAction.REQUIRES_HUMAN_REVIEW.value,
                'reason': output
            }
        elif 'approve' in output_lower and 'comment' in output_lower:
            return {
                'action': ReviewAction.APPROVE_WITH_COMMENT.value,
                'comment': output
            }
        elif 'approve' in output_lower:
            return {
                'action': ReviewAction.APPROVE_WITHOUT_COMMENT.value
            }
        else:
            return {
                'action': ReviewAction.REQUEST_CHANGES.value,
                'summary': output,
                'comments': []
            }