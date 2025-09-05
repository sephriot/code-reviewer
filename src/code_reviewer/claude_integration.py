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
        
    async def review_pr(self, pr_details: dict) -> dict:
        """Review PR using Claude Code."""
        try:
            # Prepare context for Claude
            context = self._prepare_context(pr_details)
            
            # Create temporary files for Claude to work with
            with tempfile.TemporaryDirectory() as temp_dir:
                temp_path = Path(temp_dir)
                
                # Write PR files to temp directory
                await self._write_pr_files(temp_path, pr_details)
                
                # Run Claude Code with the prompt
                result = await self._run_claude_code(temp_path, context)
                
                # Parse and validate result
                return self._parse_review_result(result)
                
        except Exception as e:
            logger.error(f"Error during Claude review: {e}")
            raise
            
    def _prepare_context(self, pr_details: dict) -> dict:
        """Prepare context information for Claude."""
        return {
            'title': pr_details.get('title', ''),
            'description': pr_details.get('body', ''),
            'author': pr_details.get('author', ''),
            'repository': pr_details.get('repository', ''),
            'branch': pr_details.get('head_branch', ''),
            'base_branch': pr_details.get('base_branch', ''),
            'changed_files': pr_details.get('changed_files', []),
            'additions': pr_details.get('additions', 0),
            'deletions': pr_details.get('deletions', 0),
        }
        
    async def _write_pr_files(self, temp_path: Path, pr_details: dict):
        """Write PR files to temporary directory for Claude to analyze."""
        files = pr_details.get('files', [])
        
        for file_info in files:
            file_path = temp_path / file_info['filename']
            file_path.parent.mkdir(parents=True, exist_ok=True)
            
            # Write the new content if available, otherwise the patch
            content = file_info.get('content', file_info.get('patch', ''))
            if content:
                file_path.write_text(content, encoding='utf-8')
                
    async def _run_claude_code(self, work_dir: Path, context: dict) -> str:
        """Run Claude Code with the review prompt."""
        try:
            # Read the prompt template
            prompt_template = self.prompt_file.read_text(encoding='utf-8')
            
            # Format prompt with context
            formatted_prompt = self._format_prompt(prompt_template, context)
            
            # Create a temporary prompt file
            prompt_file = work_dir / "review_prompt.txt"
            prompt_file.write_text(formatted_prompt, encoding='utf-8')
            
            # Run Claude Code
            cmd = ['claude-code', '--non-interactive', str(prompt_file)]
            
            process = await asyncio.create_subprocess_exec(
                *cmd,
                cwd=work_dir,
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
            
    def _format_prompt(self, template: str, context: dict) -> str:
        """Format the prompt template with context variables."""
        try:
            return template.format(**context)
        except KeyError as e:
            logger.warning(f"Missing context variable in prompt template: {e}")
            return template
            
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