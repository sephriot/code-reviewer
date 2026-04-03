"""Configuration management for the code reviewer."""

import json
import logging
import os
from dataclasses import dataclass
from pathlib import Path
from typing import List, Optional, Union

import yaml

from .models import ReviewModel


logger = logging.getLogger(__name__)

SOUND_TEMPLATE_PLACEHOLDERS = [
    ("{repo}", "Repository name (owner/repo)"),
    ("{pr_number}", "PR number"),
    ("{author}", "PR author username"),
    ("{title}", "PR title"),
]


@dataclass
class SoundFileConfig:
    """Configuration for a sound file or TTS tool."""

    tool: Optional[str] = None
    text: Optional[str] = None
    path: Optional[Path] = None

    def is_tts(self) -> bool:
        """Check if this is a TTS configuration."""
        return self.tool is not None and self.tool != ""

    def is_file(self) -> bool:
        """Check if this is a file path configuration."""
        return self.path is not None and self.path.exists()

    def get_text(self, default: str = "") -> str:
        """Get the text to speak, or default if not set."""
        return self.text if self.text else default

    def apply_template(self, context: dict) -> "SoundFileConfig":
        """Create a new config with template placeholders replaced."""
        if self.text:
            text = self.text
            for key, desc in SOUND_TEMPLATE_PLACEHOLDERS:
                text = text.replace(key, str(context.get(key[1:-1], key)))
            return SoundFileConfig(tool=self.tool, text=text, path=self.path)
        return self


@dataclass
class Config:
    """Configuration class for the code reviewer application."""

    github_token: str
    github_username: str
    prompt_file: Path
    output_format_file: Optional[Path] = None
    review_model: ReviewModel = ReviewModel.CLAUDE
    review_agent_argv: Optional[List[str]] = None
    poll_interval: int = 60
    review_timeout: int = 600
    log_level: str = "INFO"
    repositories: Optional[list] = None
    pr_authors: Optional[list] = None
    sound_enabled: bool = True
    sound_file: Optional[SoundFileConfig] = None
    approval_sound_enabled: bool = True
    approval_sound_file: Optional[SoundFileConfig] = None
    timeout_sound_enabled: bool = True
    timeout_sound_file: Optional[SoundFileConfig] = None
    merged_or_closed_sound_enabled: bool = True
    merged_or_closed_sound_file: Optional[SoundFileConfig] = None
    dry_run: bool = False
    database_path: Path = Path("data/reviews.db")
    web_enabled: bool = False
    web_host: str = "127.0.0.1"
    web_port: int = 8000
    show_thinking: bool = False
    atlas_enabled: bool = False
    own_pr_enabled: bool = False
    own_pr_ready_sound_enabled: bool = True
    own_pr_ready_sound_file: Optional[SoundFileConfig] = None
    own_pr_needs_attention_sound_enabled: bool = True
    own_pr_needs_attention_sound_file: Optional[SoundFileConfig] = None
    review_started_sound_enabled: bool = True
    review_started_sound_file: Optional[SoundFileConfig] = None
    review_started_comment_enabled: bool = False

    @classmethod
    def load(cls, config_file: Optional[str] = None, **overrides) -> "Config":
        """Load configuration from file and environment variables."""
        config_data = {}

        # Load from config file if provided
        if config_file:
            config_path = Path(config_file)
            if config_path.exists():
                with open(config_path, "r") as f:
                    config_data = yaml.safe_load(f) or {}
            else:
                raise FileNotFoundError(f"Config file not found: {config_file}")

        # Override with environment variables
        env_mappings = {
            "GITHUB_TOKEN": "github_token",
            "GITHUB_USERNAME": "github_username",
            "PROMPT_FILE": "prompt_file",
            "OUTPUT_FORMAT_FILE": "output_format_file",
            "REVIEW_MODEL": "review_model",
            "REVIEW_TIMEOUT": "review_timeout",
            "POLL_INTERVAL": "poll_interval",
            "LOG_LEVEL": "log_level",
            "REPOSITORIES": "repositories",
            "PR_AUTHORS": "pr_authors",
            "SOUND_ENABLED": "sound_enabled",
            "SOUND_FILE": "sound_file",
            "APPROVAL_SOUND_ENABLED": "approval_sound_enabled",
            "APPROVAL_SOUND_FILE": "approval_sound_file",
            "TIMEOUT_SOUND_ENABLED": "timeout_sound_enabled",
            "TIMEOUT_SOUND_FILE": "timeout_sound_file",
            "MERGED_OR_CLOSED_SOUND_ENABLED": "merged_or_closed_sound_enabled",
            "MERGED_OR_CLOSED_SOUND_FILE": "merged_or_closed_sound_file",
            "DRY_RUN": "dry_run",
            "DATABASE_PATH": "database_path",
            "WEB_ENABLED": "web_enabled",
            "WEB_HOST": "web_host",
            "WEB_PORT": "web_port",
            "SHOW_THINKING": "show_thinking",
            "ATLAS_ENABLED": "atlas_enabled",
            "OWN_PR_ENABLED": "own_pr_enabled",
            "OWN_PR_READY_SOUND_ENABLED": "own_pr_ready_sound_enabled",
            "OWN_PR_READY_SOUND_FILE": "own_pr_ready_sound_file",
            "OWN_PR_NEEDS_ATTENTION_SOUND_ENABLED": "own_pr_needs_attention_sound_enabled",
            "OWN_PR_NEEDS_ATTENTION_SOUND_FILE": "own_pr_needs_attention_sound_file",
            "REVIEW_AGENT_ARGV": "review_agent_argv",
            "REVIEW_STARTED_SOUND_ENABLED": "review_started_sound_enabled",
            "REVIEW_STARTED_SOUND_FILE": "review_started_sound_file",
            "REVIEW_STARTED_COMMENT_ENABLED": "review_started_comment_enabled",
        }

        for env_var, config_key in env_mappings.items():
            value = os.getenv(env_var)
            if value:
                if config_key in ["poll_interval", "web_port", "review_timeout"]:
                    config_data[config_key] = int(value)
                elif config_key in [
                    "sound_enabled",
                    "approval_sound_enabled",
                    "timeout_sound_enabled",
                    "merged_or_closed_sound_enabled",
                    "dry_run",
                    "web_enabled",
                    "show_thinking",
                    "atlas_enabled",
                    "own_pr_enabled",
                    "own_pr_ready_sound_enabled",
                    "own_pr_needs_attention_sound_enabled",
                    "review_started_sound_enabled",
                    "review_started_comment_enabled",
                ]:
                    config_data[config_key] = value.lower() in (
                        "true",
                        "1",
                        "yes",
                        "on",
                    )
                elif config_key in [
                    "sound_file",
                    "approval_sound_file",
                    "timeout_sound_file",
                    "merged_or_closed_sound_file",
                    "own_pr_ready_sound_file",
                    "own_pr_needs_attention_sound_file",
                    "review_started_sound_file",
                ]:
                    # Keep as string for later parsing in sound file fields processing
                    config_data[config_key] = value
                elif config_key in ["repositories", "pr_authors"]:
                    # Parse comma-separated lists
                    items = [item.strip() for item in value.split(",") if item.strip()]
                    config_data[config_key] = items if items else None
                elif config_key == "review_agent_argv":
                    try:
                        parsed = json.loads(value)
                    except json.JSONDecodeError as exc:
                        raise ValueError(
                            "REVIEW_AGENT_ARGV must be a JSON array of strings, "
                            f'e.g. \'["agent","--print","--output-format","json","--trust"]\': {exc}'
                        ) from exc
                    config_data[config_key] = parsed
                else:
                    config_data[config_key] = value

        # Support legacy environment variables for backward compatibility
        legacy_sound_enabled = os.getenv("OUTDATED_SOUND_ENABLED")
        if legacy_sound_enabled and "merged_or_closed_sound_enabled" not in config_data:
            config_data["merged_or_closed_sound_enabled"] = (
                legacy_sound_enabled.lower() in ("true", "1", "yes", "on")
            )

        legacy_sound_file = os.getenv("OUTDATED_SOUND_FILE")
        if legacy_sound_file and "merged_or_closed_sound_file" not in config_data:
            config_data["merged_or_closed_sound_file"] = Path(legacy_sound_file)

        # Override with function parameters
        for key, value in overrides.items():
            if value is not None:
                config_data[key] = value

        if "review_timeout" in config_data:
            timeout_value = config_data["review_timeout"]
            try:
                timeout_int = int(timeout_value)
            except (TypeError, ValueError):
                raise ValueError("review_timeout must be an integer number of seconds")
            if timeout_int < 0:
                raise ValueError("review_timeout must be non-negative")
            # A value of 0 disables the timeout
            config_data["review_timeout"] = timeout_int

        # Normalize review model selection
        config_data["review_model"] = cls._normalize_review_model(
            config_data.get("review_model", ReviewModel.CLAUDE)
        )

        if (
            "review_agent_argv" in config_data
            and config_data["review_agent_argv"] is not None
        ):
            argv = config_data["review_agent_argv"]
            if not isinstance(argv, list) or not all(isinstance(x, str) for x in argv):
                raise ValueError("review_agent_argv must be a list of strings")
            if len(argv) == 0:
                raise ValueError("review_agent_argv cannot be empty when set")

        # Validate required fields
        required_fields = ["github_token", "github_username"]
        for field in required_fields:
            if not config_data.get(field):
                raise ValueError(f"Required configuration field missing: {field}")

        # Handle prompt_file
        if "prompt_file" in config_data:
            prompt_file = Path(config_data["prompt_file"])
        else:
            # Default to prompts/review_prompt.txt
            prompt_file = Path("prompts/review_prompt.txt")

        if not prompt_file.exists():
            # Create default prompt file
            prompt_file.parent.mkdir(parents=True, exist_ok=True)
            cls._create_default_prompt(prompt_file)

        config_data["prompt_file"] = prompt_file

        # Handle output_format_file
        if "output_format_file" in config_data and config_data["output_format_file"]:
            output_format_file = Path(config_data["output_format_file"])
            if not output_format_file.exists():
                raise FileNotFoundError(
                    f"Output format file not found: {output_format_file}"
                )
            config_data["output_format_file"] = output_format_file

        # Handle sound files - convert to SoundFileConfig or keep as Path
        sound_file_fields = [
            "sound_file",
            "approval_sound_file",
            "timeout_sound_file",
            "merged_or_closed_sound_file",
            "own_pr_ready_sound_file",
            "own_pr_needs_attention_sound_file",
            "review_started_sound_file",
        ]
        for field in sound_file_fields:
            if field in config_data and config_data[field] is not None:
                value = config_data[field]
                if isinstance(value, str):
                    config_data[field] = cls._parse_sound_file(value)
                elif isinstance(value, Path):
                    config_data[field] = SoundFileConfig(path=value)

        # Handle path conversions for non-sound fields
        for path_field in [
            "prompt_file",
            "output_format_file",
            "database_path",
        ]:
            if path_field in config_data and config_data[path_field] is not None:
                if not isinstance(config_data[path_field], Path):
                    config_data[path_field] = Path(config_data[path_field])

        return cls(**config_data)

    @staticmethod
    def _parse_sound_file(value: str) -> "SoundFileConfig":
        """Parse sound file config from string format: 'tool:text' or 'path/to/file'."""
        if ":" in value:
            parts = value.split(":", 1)
            tool = parts[0].strip().lower()
            text = parts[1].strip() if len(parts) > 1 else ""
            return SoundFileConfig(tool=tool, text=text)
        else:
            return SoundFileConfig(tool=None, text=None, path=Path(value))

    @staticmethod
    def _normalize_review_model(value) -> ReviewModel:
        """Convert string or enum input into ReviewModel."""
        if value is None:
            return ReviewModel.CLAUDE
        if isinstance(value, ReviewModel):
            return value
        if isinstance(value, str):
            candidate = value.strip().upper()
            try:
                return ReviewModel[candidate]
            except KeyError:
                try:
                    return ReviewModel(candidate)
                except ValueError as exc:
                    valid = ", ".join(model.value for model in ReviewModel)
                    raise ValueError(
                        f"Unsupported review model '{value}'. Choose from: {valid}."
                    ) from exc
        valid = ", ".join(model.value for model in ReviewModel)
        raise ValueError(f"Unsupported review model '{value}'. Choose from: {valid}.")

    @staticmethod
    def _create_default_prompt(prompt_file: Path):
        """Create a default prompt file."""
        default_prompt = """# Code Review Prompt

You are an experienced software engineer conducting a code review. Please analyze the provided PR and respond with a JSON object in the following format:

```json
{
  "action": "approve_with_comment" | "approve_without_comment" | "request_changes" | "requires_human_review",
  "comment": "Optional comment for approval",
  "summary": "Summary for requested changes", 
  "reason": "Reason why human review is needed",
  "comments": [
    {
      "file": "path/to/file.py",
      "line": 42,
      "message": "Specific feedback for this line"
    }
  ]
}
```

## PR Information:
- **Title:** {title}
- **Description:** {description}
- **Author:** {author}
- **Repository:** {repository}
- **Branch:** {branch} -> {base_branch}
- **Files Changed:** {changed_files}
- **Additions:** {additions} lines
- **Deletions:** {deletions} lines

## Review Guidelines:
1. Check for code quality and best practices
2. Look for potential bugs or security issues
3. Verify tests are included for new functionality
4. Ensure documentation is updated if needed
5. Check for proper error handling
6. Verify performance considerations

## When to Use Each Action:
- **approve_with_comment**: Code is good but has minor suggestions
- **approve_without_comment**: Code is perfect and ready to merge
- **request_changes**: Code has issues that must be fixed before merging
- **requires_human_review**: PR is too complex, has architectural implications, or needs domain expertise

Please review the files in the current directory and provide your assessment."""

        prompt_file.write_text(default_prompt, encoding="utf-8")
        logger.info(f"Created default prompt file: {prompt_file}")

    def setup_logging(self):
        """Set up logging configuration."""
        logging.basicConfig(
            level=getattr(logging, self.log_level.upper()),
            format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
            datefmt="%Y-%m-%d %H:%M:%S",
        )
