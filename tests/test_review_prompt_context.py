#!/usr/bin/env python3
"""Tests for propagating previous pending approval context into review prompts."""

from pathlib import Path

import pytest

# Ensure src on path
import sys
sys.path.insert(0, str(Path(__file__).parent.parent / 'src'))

from code_reviewer.models import PRInfo, ReviewModel, ReviewAction
from code_reviewer.llm_integration import LLMIntegration


@pytest.mark.asyncio
async def test_review_prompt_includes_previous_pending_context(tmp_path: Path):
    """LLMIntegration should append previous pending approval details to the prompt."""

    prompt_file = tmp_path / "prompt.txt"
    prompt_file.write_text("Prompt Body")

    class CapturingIntegration(LLMIntegration):
        def __init__(self):
            super().__init__(prompt_file, ReviewModel.CLAUDE)
            self.captured_prompt = None
            self.captured_previous = None
            self.captured_context = None
            self.captured_block = None

        async def _run_model_cli(self, pr_info: PRInfo, previous_pending=None):  # type: ignore[override]
            prompt_template = self.prompt_file.read_text(encoding="utf-8")
            previous_context = self._format_previous_pending(previous_pending) if previous_pending else ""
            context_block = (
                f"\n\nPrevious pending review details for earlier commit {previous_pending.get('head_sha', '')[:8]}:\n{previous_context}\n"
                if previous_context
                else ""
            )
            full_prompt = (
                f"Please review this GitHub pull request: {pr_info.url}\n\n"
                f"Repository: {pr_info.repository_name}\n"
                f"PR Number: #{pr_info.number}\n\n"
                f"{prompt_template}\n"
                f"{context_block}"
                f"\nPlease analyze the pull request at {pr_info.url} and provide your review following the instructions above."
            )
            self.captured_previous = previous_pending
            self.captured_prompt = full_prompt
            self.captured_context = previous_context
            self.captured_block = context_block
            return '{"action": "approve_without_comment"}'

    integration = CapturingIntegration()

    pr_info = PRInfo(
        id=1,
        number=42,
        repository=["owner", "repo"],
        url="https://github.com/owner/repo/pull/42",
        title="Update feature",
        author="alice",
        head_sha="cafebabe",
        base_sha="deadbeef",
    )

    previous_pending = {
        "head_sha": "deadbeef01",
        "created_at": "2025-10-09T12:00:00Z",
        "review_action": "approve_with_comment",
        "display_review_comment": "Consider adding regression tests.",
        "display_review_summary": "Tests are missing for new endpoints.",
        "review_reason": "",
        "inline_comments": [
            {"file": "api/routes.py", "line": 42, "message": "Validate payload schema."}
        ],
    }

    result = await integration.review_pr(pr_info, previous_pending=previous_pending)

    assert result.action == ReviewAction.APPROVE_WITHOUT_COMMENT
    assert integration.captured_previous is previous_pending
    assert integration.captured_prompt is not None
    assert integration.captured_context
    assert integration.captured_block
    assert "Previous pending review details" in integration.captured_block
    assert integration.captured_block in integration.captured_prompt
    assert "Consider adding regression tests." in integration.captured_prompt
    assert "api/routes.py:42" in integration.captured_prompt
