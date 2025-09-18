"""Language model integration for PR reviews."""

import asyncio
import json
import logging
import re
from pathlib import Path
from typing import List

from .models import PRInfo, ReviewResult, ReviewAction, ReviewModel


logger = logging.getLogger(__name__)


class LLMOutputParseError(Exception):
    """Raised when the model output cannot be parsed as valid JSON."""

    def __init__(self, message: str, raw_output: str):
        super().__init__(message)
        self.raw_output = raw_output


class LLMIntegration:
    """Execute PR reviews using the configured language model CLI."""

    _CLI_COMMANDS = {
        ReviewModel.CLAUDE: ["claude", "--print"],
        ReviewModel.CODEX: ["codex", "exec", "--sandbox", "danger-full-access", "--skip-git-repo-check"],
    }

    def __init__(self, prompt_file: Path, model: ReviewModel):
        self.prompt_file = prompt_file
        self.model = model

    async def review_pr(self, pr_info: PRInfo) -> ReviewResult:
        """Review a PR using the selected language model CLI."""
        try:
            result = await self._run_model_cli(pr_info)
            return self._parse_review_result(result)
        except Exception as exc:  # pragma: no cover - pass through for upstream handling
            logger.error(f"Error during {self.model.value} review: {exc}")
            raise

    async def _run_model_cli(self, pr_info: PRInfo) -> str:
        """Execute the configured CLI and return raw output."""
        prompt_template = self.prompt_file.read_text(encoding="utf-8")

        pr_url = pr_info.url
        repo_name = pr_info.repository_name
        pr_number = pr_info.number

        full_prompt = (
            f"Please review this GitHub pull request: {pr_url}\n\n"
            f"Repository: {repo_name}\n"
            f"PR Number: #{pr_number}\n\n"
            f"{prompt_template}\n\n"
            f"Please analyze the pull request at {pr_url} and provide your review following the instructions above."
        )

        cmd = self._build_command()

        process = await asyncio.create_subprocess_exec(
            *cmd,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )

        stdout, stderr = await process.communicate(input=full_prompt.encode())

        if process.returncode != 0:
            raise RuntimeError(
                f"{self.model.value} CLI failed ({' '.join(cmd)}), stderr: {stderr.decode()}"
            )

        return stdout.decode()

    def _build_command(self) -> List[str]:
        """Resolve the CLI command for the configured model."""
        try:
            return self._CLI_COMMANDS[self.model]
        except KeyError:  # pragma: no cover - defensive
            raise ValueError(f"Unsupported review model: {self.model}")

    def _parse_review_result(self, raw_output: str) -> ReviewResult:
        """Parse model output into a structured review result."""
        json_pattern = r"\{[^{}]*(?:\{[^{}]*\}[^{}]*)*\}"
        json_matches = re.findall(json_pattern, raw_output, re.DOTALL)

        for candidate in json_matches:
            candidate = candidate.strip()
            try:
                result_dict = json.loads(candidate)
                self._validate_review_result(result_dict)
                logger.info(f"Successfully parsed JSON: {result_dict}")
                return ReviewResult.from_dict(result_dict)
            except json.JSONDecodeError:
                logger.debug("Failed to parse JSON candidate", exc_info=True)
                continue

        for line in raw_output.strip().split("\n"):
            line = line.strip()
            if line.startswith("{") and line.endswith("}"):
                try:
                    result_dict = json.loads(line)
                    self._validate_review_result(result_dict)
                    logger.info(f"Successfully parsed line JSON: {result_dict}")
                    return ReviewResult.from_dict(result_dict)
                except json.JSONDecodeError:
                    logger.debug("Failed to parse line JSON", exc_info=True)
                    continue

        error_msg = (
            f"{self.model.value} output does not contain valid JSON. Output length: {len(raw_output)}"
        )
        logger.error(error_msg)
        logger.error(f"Model output: {raw_output}")
        raise LLMOutputParseError(error_msg, raw_output)

    @staticmethod
    def _validate_review_result(result: dict) -> None:
        """Validate the parsed review result structure."""
        if "action" not in result:
            raise ValueError("Missing required field: action")

        try:
            ReviewAction(result["action"])
        except ValueError as exc:
            raise ValueError(f"Invalid action: {result['action']}") from exc

