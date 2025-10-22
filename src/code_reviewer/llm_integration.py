"""Language model integration for PR reviews."""

import asyncio
import json
import logging
import re
import sys
import tempfile
import uuid
from pathlib import Path
from typing import Any, Dict, List, Optional, TextIO

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

    async def review_pr(
        self,
        pr_info: PRInfo,
        *,
        timeout: Optional[int] = None,
        previous_pending: Optional[Dict[str, Any]] = None,
    ) -> ReviewResult:
        """Review a PR using the selected language model CLI."""
        try:
            run_coro = self._run_model_cli(pr_info, previous_pending)
            if timeout and timeout > 0:
                result = await asyncio.wait_for(run_coro, timeout=timeout)
            else:
                result = await run_coro
            return self._parse_review_result(result)
        except asyncio.TimeoutError:
            logger.warning(
                "%s review exceeded timeout after %s seconds",
                self.model.value,
                timeout,
            )
            raise
        except Exception as exc:  # pragma: no cover - pass through for upstream handling
            logger.error(f"Error during {self.model.value} review: {exc}")
            raise

    async def _run_model_cli(self, pr_info: PRInfo, previous_pending: Optional[Dict[str, Any]]) -> str:
        """Execute the configured CLI and return raw output."""
        prompt_template = self.prompt_file.read_text(encoding="utf-8")

        pr_url = pr_info.url
        repo_name = pr_info.repository_name
        pr_number = pr_info.number

        previous_context = self._format_previous_pending(previous_pending) if previous_pending else ""

        context_block = (
            f"\n\nPrevious pending review details for earlier commit {previous_pending.get('head_sha', '')[:8]}:\n{previous_context}\n"
            if previous_context
            else ""
        )

        full_prompt = (
            f"Please review this GitHub pull request: {pr_url}\n\n"
            f"Repository: {repo_name}\n"
            f"PR Number: #{pr_number}\n\n"
            f"{prompt_template}\n"
            f"{context_block}"
            f"\nPlease analyze the pull request at {pr_url} and provide your review following the instructions above."
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

        try:
            await process.wait()
            await asyncio.gather(stdout_task, stderr_task)
        except asyncio.CancelledError:
            logger.warning(
                "%s CLI invocation cancelled; terminating subprocess",
                self.model.value,
            )
            process.terminate()
            try:
                await asyncio.wait_for(process.wait(), timeout=5)
            except asyncio.TimeoutError:
                logger.debug("Force killing %s subprocess", self.model.value)
                process.kill()
                await process.wait()
            stdout_task.cancel()
            stderr_task.cancel()
            await asyncio.gather(stdout_task, stderr_task, return_exceptions=True)
            if codex_output_path and codex_output_path.exists():
                codex_output_path.unlink(missing_ok=True)
            raise

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

    def _format_previous_pending(self, previous_pending: Dict[str, Any]) -> str:
        """Convert a pending approval record into prompt-friendly text."""
        if not previous_pending:
            return ""

        lines: List[str] = []

        head_sha = previous_pending.get('head_sha') or ''
        head_display = head_sha[:8] if head_sha else 'unknown'
        created_at = previous_pending.get('created_at')
        header = f"- Commit: {head_display}"
        if created_at:
            header += f" (recorded at {created_at})"
        lines.append(header)

        action = previous_pending.get('review_action')
        if action:
            lines.append(f"- Suggested action: {action}")

        comment = (previous_pending.get('display_review_comment') or '').strip()
        if comment:
            lines.append("Overall comment:\n" + comment)

        summary = (previous_pending.get('display_review_summary') or '').strip()
        if summary:
            lines.append("Summary of requested changes:\n" + summary)

        reason = (previous_pending.get('review_reason') or '').strip()
        if reason:
            lines.append("Reason for human attention:\n" + reason)

        inline_comments = previous_pending.get('inline_comments') or []
        if inline_comments:
            formatted = []
            for idx, inline in enumerate(inline_comments, 1):
                file_path = inline.get('file', 'unknown file')
                line_no = inline.get('line')
                location = f"{file_path}:{line_no}" if line_no is not None else file_path
                message = (inline.get('message') or '').strip()
                formatted.append(f"{idx}. {location} — {message}")
            lines.append("Inline feedback items:\n" + "\n".join(formatted))

        return "\n".join(lines).strip()

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

    @staticmethod
    def _strip_markdown_fences(text: str) -> str:
        """Remove markdown code fences from text if present.

        Handles patterns like:
        ```json
        {...}
        ```
        or
        ```
        {...}
        ```

        Also handles cases where there's text before/after the fence.
        """
        # Pattern to match markdown code fences with optional language specifier
        # This pattern doesn't require the fence to be at start/end of string
        fence_pattern = r'```(?:json)?\s*\n(.*?)\n```'
        match = re.search(fence_pattern, text, re.DOTALL)

        if match:
            return match.group(1)

        return text

    def _parse_review_result(self, raw_output: str) -> ReviewResult:
        """Parse model output into a structured review result."""
        # Strip markdown code fences if present
        cleaned_output = self._strip_markdown_fences(raw_output)

        # Strategy 1: Try to parse the entire cleaned output as JSON
        try:
            result_dict = json.loads(cleaned_output.strip())
            self._validate_review_result(result_dict)
            logger.info(f"Successfully parsed JSON from cleaned output")
            return ReviewResult.from_dict(result_dict)
        except json.JSONDecodeError:
            logger.debug("Failed to parse entire cleaned output as JSON")

        # Strategy 2: Extract complete JSON objects by matching braces
        json_candidates = self._extract_json_objects(cleaned_output)

        for candidate in json_candidates:
            try:
                result_dict = json.loads(candidate)
                self._validate_review_result(result_dict)
                logger.info(f"Successfully parsed JSON from extracted object")
                return ReviewResult.from_dict(result_dict)
            except json.JSONDecodeError:
                logger.debug("Failed to parse JSON candidate", exc_info=True)
                continue

        # Strategy 3: Try line-by-line for simple single-line JSON
        for line in cleaned_output.strip().split("\n"):
            line = line.strip()
            if line.startswith("{") and line.endswith("}"):
                try:
                    result_dict = json.loads(line)
                    self._validate_review_result(result_dict)
                    logger.info(f"Successfully parsed line JSON")
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
    def _extract_json_objects(text: str) -> List[str]:
        """Extract complete JSON objects from text by matching braces.

        This handles deeply nested JSON by counting opening/closing braces.
        """
        candidates = []
        i = 0
        while i < len(text):
            if text[i] == '{':
                # Found potential start of JSON object
                brace_count = 0
                start = i
                j = i
                in_string = False
                escape_next = False

                while j < len(text):
                    char = text[j]

                    if escape_next:
                        escape_next = False
                        j += 1
                        continue

                    if char == '\\':
                        escape_next = True
                        j += 1
                        continue

                    if char == '"':
                        in_string = not in_string
                    elif not in_string:
                        if char == '{':
                            brace_count += 1
                        elif char == '}':
                            brace_count -= 1
                            if brace_count == 0:
                                # Found complete JSON object
                                candidates.append(text[start:j+1])
                                i = j + 1
                                break

                    j += 1

                if brace_count != 0:
                    # Incomplete JSON object, move to next character
                    i += 1
            else:
                i += 1

        return candidates

    @staticmethod
    def _validate_review_result(result: dict) -> None:
        """Validate the parsed review result structure."""
        if "action" not in result:
            raise ValueError("Missing required field: action")

        try:
            ReviewAction(result["action"])
        except ValueError as exc:
            raise ValueError(f"Invalid action: {result['action']}") from exc
