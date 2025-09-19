"""Language model integration for PR reviews."""

import asyncio
import json
import logging
import re
import sys
import tempfile
import uuid
from pathlib import Path
from typing import List, Optional, TextIO

from .models import PRInfo, ReviewResult, ReviewAction, ReviewModel


logger = logging.getLogger(__name__)


class LLMOutputParseError(Exception):
    """Raised when the model output cannot be parsed as valid JSON."""

    def __init__(self, message: str, raw_output: str):
        super().__init__(message)
        self.raw_output = raw_output


class LLMIntegration:
    """Execute PR reviews using the configured language model CLI."""

    _CODEX_OUTPUT_FILE = "codex_review_output.json"


    _CLI_COMMANDS = {
        ReviewModel.CLAUDE: ["claude", "--print"],
        ReviewModel.CODEX: [
            "codex",
            "exec",
            "--sandbox",
            "danger-full-access",
            "--skip-git-repo-check",
            "--output-last-message",
        ],
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

        codex_output_path = None
        if self.model is ReviewModel.CODEX:
            codex_output_path = self._prepare_codex_output_path()

        cmd = self._build_command(codex_output_path)

        process = await asyncio.create_subprocess_exec(
            *cmd,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )

        stdout_buffer: List[str] = []
        stderr_buffer: List[str] = []

        assert process.stdin is not None  # for type checkers
        process.stdin.write(full_prompt.encode())
        await process.stdin.drain()
        process.stdin.close()
        if hasattr(process.stdin, "wait_closed"):
            await process.stdin.wait_closed()

        stdout_task = asyncio.create_task(
            self._stream_subprocess_output(
                process.stdout, stdout_buffer, output_stream=sys.stdout
            )
        )
        stderr_task = asyncio.create_task(
            self._stream_subprocess_output(
                process.stderr, stderr_buffer, output_stream=sys.stderr
            )
        )

        await process.wait()
        await asyncio.gather(stdout_task, stderr_task)

        if process.returncode != 0:
            stderr_text = "".join(stderr_buffer)
            raise RuntimeError(
                f"{self.model.value} CLI failed ({' '.join(cmd)}), stderr: {stderr_text}"
            )

        if self.model is ReviewModel.CODEX:
            if codex_output_path is None:
                raise RuntimeError("Codex output path was not configured")
            if not codex_output_path.exists():
                raise RuntimeError(
                    f"Expected Codex output file missing: {codex_output_path}"
                )
            try:
                raw_output = codex_output_path.read_text(encoding="utf-8")
            finally:
                codex_output_path.unlink(missing_ok=True)
        else:
            raw_output = "".join(stdout_buffer)

        return raw_output

    def _build_command(self, codex_output_path: Optional[Path] = None) -> List[str]:
        """Resolve the CLI command for the configured model."""
        try:
            command = list(self._CLI_COMMANDS[self.model])
        except KeyError:  # pragma: no cover - defensive
            raise ValueError(f"Unsupported review model: {self.model}")

        if self.model is ReviewModel.CODEX:
            output_target = codex_output_path or Path(self._CODEX_OUTPUT_FILE)
            command.append(str(output_target))

        return command

    def _prepare_codex_output_path(self) -> Path:
        """Create a unique path for Codex output to avoid stale data."""
        base_name = Path(self._CODEX_OUTPUT_FILE)
        suffix = base_name.suffix or ".json"
        prefix = base_name.stem or "codex_review_output"
        output_path = (
            Path(tempfile.gettempdir())
            / f"{prefix}_{uuid.uuid4().hex}{suffix}"
        )
        if output_path.exists():
            output_path.unlink()
        return output_path

    async def _stream_subprocess_output(
        self,
        stream: Optional[asyncio.StreamReader],
        buffer: List[str],
        *,
        output_stream: TextIO,
    ) -> None:
        """Stream subprocess output to the provided IO while retaining it for parsing."""
        if stream is None:
            return

        while True:
            line = await stream.readline()
            if not line:
                break
            text = line.decode(errors="replace")
            buffer.append(text)
            output_stream.write(text)
            output_stream.flush()

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
