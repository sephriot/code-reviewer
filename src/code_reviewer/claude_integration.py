"""Claude Code integration for PR reviews."""

import asyncio
import json
import logging
import subprocess
import tempfile
from pathlib import Path
from .models import PRInfo, ReviewResult, ReviewAction


logger = logging.getLogger(__name__)


class ClaudeIntegration:
    def __init__(self, prompt_file: Path):
        self.prompt_file = prompt_file
        
    async def review_pr(self, pr_info: PRInfo) -> ReviewResult:
        """Review PR using Claude Code - Claude Code will fetch all PR information."""
        try:
            # Run Claude Code with the PR URL - Claude Code will handle all data fetching
            result = await self._run_claude_code(pr_info)

            # Parse and validate result
            return self._parse_review_result(result)
                
        except Exception as e:
            logger.error(f"Error during Claude review: {e}")
            raise
            
                
    async def _run_claude_code(self, pr_info: PRInfo) -> str:
        """Run Claude Code with the review prompt and PR URL."""
        try:
            # Read the prompt template
            prompt_template = self.prompt_file.read_text(encoding='utf-8')
            
            # Build the full prompt with PR URL
            pr_url = pr_info.url
            repo_name = pr_info.repository_name
            pr_number = pr_info.number
            
            full_prompt = f"""Please review this GitHub pull request: {pr_url}

Repository: {repo_name}
PR Number: #{pr_number}

{prompt_template}

Please analyze the pull request at {pr_url} and provide your review following the instructions above."""

            # Run Claude Code directly with the prompt
            cmd = ['claude', '--print']
            
            process = await asyncio.create_subprocess_exec(
                *cmd,
                stdin=asyncio.subprocess.PIPE,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE
            )
            
            stdout, stderr = await process.communicate(input=full_prompt.encode())
            
            if process.returncode != 0:
                raise RuntimeError(f"Claude Code failed: {stderr.decode()}")
                
            return stdout.decode()
            
        except Exception as e:
            logger.error(f"Error running Claude Code: {e}")
            raise
            
            
    def _parse_review_result(self, claude_output: str) -> ReviewResult:
        """Parse Claude's review output into structured result."""
        try:
            # Look for JSON output in Claude's response - try multiple approaches
            
            # Approach 1: Look for complete JSON blocks (including multiline)
            import re
            json_pattern = r'\{[^{}]*(?:\{[^{}]*\}[^{}]*)*\}'
            json_matches = re.findall(json_pattern, claude_output, re.DOTALL)
            
            for json_str in json_matches:
                json_str = json_str.strip()
                try:
                    result_dict = json.loads(json_str)
                    self._validate_review_result(result_dict)
                    logger.info(f"Successfully parsed JSON: {result_dict}")
                    return ReviewResult.from_dict(result_dict)
                except json.JSONDecodeError as e:
                    logger.debug(f"Failed to parse JSON candidate: {e}")
                    continue
            
            # Approach 2: Look for line-based JSON
            lines = claude_output.strip().split('\n')
            for line in lines:
                line = line.strip()
                if line.startswith('{') and line.endswith('}'):
                    try:
                        result_dict = json.loads(line)
                        self._validate_review_result(result_dict)
                        logger.info(f"Successfully parsed line JSON: {result_dict}")
                        return ReviewResult.from_dict(result_dict)
                    except json.JSONDecodeError as e:
                        logger.debug(f"Failed to parse line JSON: {e}")
                        continue
                        
            # If no JSON found, log the issue and use fallback
            logger.warning(f"No valid JSON found in Claude output, falling back to text parsing. Output length: {len(claude_output)}")
            logger.debug(f"Claude output preview: {claude_output[:500]}...")
            return self._parse_text_result(claude_output)
            
        except Exception as e:
            logger.error(f"Error parsing Claude output: {e}")
            # Return default safe action
            return ReviewResult(
                action=ReviewAction.REQUEST_CHANGES,
                summary='Failed to parse review result',
                comments=[]
            )
            
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
            
    def _parse_text_result(self, output: str) -> ReviewResult:
        """Parse text-based review result as fallback."""
        logger.warning("Using text fallback parsing - this indicates Claude didn't follow JSON-only instructions")
        
        # For fast review prompt, we should only have two actions
        # If we're falling back to text parsing, be more conservative
        output_lower = output.lower()
        
        if 'human' in output_lower and 'review' in output_lower:
            # Extract a brief reason from the output instead of using the entire text
            brief_reason = "Requires human review (extracted from text response)"
            return ReviewResult(
                action=ReviewAction.REQUIRES_HUMAN_REVIEW,
                reason=brief_reason
            )
        elif 'approve' in output_lower:
            # Don't use APPROVE_WITH_COMMENT in text fallback to avoid long comments
            # The fast prompt should only approve without comments anyway
            return ReviewResult(
                action=ReviewAction.APPROVE_WITHOUT_COMMENT
            )
        else:
            # Default to requiring human review if unclear
            return ReviewResult(
                action=ReviewAction.REQUIRES_HUMAN_REVIEW,
                reason="Could not parse review decision - requires human review"
            )