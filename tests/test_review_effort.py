"""Tests for the configurable review effort level (Claude `--effort`)."""

from pathlib import Path

from code_reviewer.config import Config
from code_reviewer.llm_integration import LLMIntegration
from code_reviewer.models import ReviewModel


PROMPT = Path("prompts/review_prompt.txt")


def test_claude_valid_effort_in_command():
    integration = LLMIntegration(PROMPT, ReviewModel.CLAUDE, effort="high")
    cmd = integration._build_command()
    assert "--effort" in cmd
    assert cmd[cmd.index("--effort") + 1] == "high"
    assert integration.effort == "high"
    assert integration.effort_message == "Using effort: high"


def test_claude_effort_is_case_insensitive():
    integration = LLMIntegration(PROMPT, ReviewModel.CLAUDE, effort="High")
    cmd = integration._build_command()
    assert cmd[cmd.index("--effort") + 1] == "high"


def test_claude_invalid_effort_falls_back_to_default():
    integration = LLMIntegration(PROMPT, ReviewModel.CLAUDE, effort="ultra")
    cmd = integration._build_command()
    assert "--effort" not in cmd
    assert integration.effort is None
    assert integration.effort_message is not None
    assert "ultra" in integration.effort_message


def test_codex_effort_is_incompatible():
    integration = LLMIntegration(PROMPT, ReviewModel.CODEX, effort="high")
    cmd = integration._build_command()
    assert "--effort" not in cmd
    assert integration.effort is None
    assert integration.effort_message is not None
    assert "CODEX" in integration.effort_message


def test_effort_unset_passes_no_flag():
    integration = LLMIntegration(PROMPT, ReviewModel.CLAUDE)
    cmd = integration._build_command()
    assert "--effort" not in cmd
    assert integration.effort is None
    assert integration.effort_message is None


def test_config_loads_review_effort_from_env(monkeypatch):
    monkeypatch.setenv("GITHUB_TOKEN", "t")
    monkeypatch.setenv("GITHUB_USERNAME", "u")
    monkeypatch.setenv("REVIEW_EFFORT", "High")

    c = Config.load()
    assert c.review_effort == "high"
