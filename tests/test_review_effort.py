"""Tests for the configurable review effort level (Claude `--effort`)."""

from pathlib import Path

import pytest

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


def test_codex_uses_workspace_write_sandbox():
    integration = LLMIntegration(PROMPT, ReviewModel.CODEX)
    cmd = integration._build_command()

    assert cmd[cmd.index("--sandbox") + 1] == "workspace-write"
    assert "danger-full-access" not in cmd


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


def test_config_loads_startup_sounds_enabled_from_env(monkeypatch):
    monkeypatch.setenv("GITHUB_TOKEN", "t")
    monkeypatch.setenv("GITHUB_USERNAME", "u")
    monkeypatch.setenv("STARTUP_SOUNDS_ENABLED", "false")

    c = Config.load()
    assert c.startup_sounds_enabled is False


def test_config_loads_review_tool_from_env(monkeypatch):
    monkeypatch.setenv("GITHUB_TOKEN", "t")
    monkeypatch.setenv("GITHUB_USERNAME", "u")
    monkeypatch.setenv("REVIEW_TOOL", "CODEX")

    c = Config.load()
    assert c.review_tool is ReviewModel.CODEX


def test_config_accepts_legacy_review_model_env(monkeypatch):
    monkeypatch.setenv("GITHUB_TOKEN", "t")
    monkeypatch.setenv("GITHUB_USERNAME", "u")
    monkeypatch.setenv("REVIEW_MODEL", "AGENT")

    c = Config.load()
    assert c.review_tool is ReviewModel.AGENT


def test_claude_model_in_command():
    integration = LLMIntegration(PROMPT, ReviewModel.CLAUDE, claude_model="Sonnet")
    cmd = integration._build_command()

    assert "--model" in cmd
    assert cmd[cmd.index("--model") + 1] == "sonnet"
    assert integration.claude_model == "sonnet"


def test_claude_model_override_in_command():
    integration = LLMIntegration(PROMPT, ReviewModel.CLAUDE, claude_model="opus")
    cmd = integration._build_command(claude_model_override="fable")

    assert cmd[cmd.index("--model") + 1] == "fable"


def test_non_claude_model_does_not_get_claude_model_flag():
    integration = LLMIntegration(PROMPT, ReviewModel.CODEX, claude_model="opus")
    cmd = integration._build_command()

    assert "--model" not in cmd


def test_invalid_claude_model_raises():
    with pytest.raises(ValueError, match="Unsupported Claude model"):
        LLMIntegration(PROMPT, ReviewModel.CLAUDE, claude_model="haiku")


def test_config_loads_claude_model_from_env(monkeypatch):
    monkeypatch.setenv("GITHUB_TOKEN", "t")
    monkeypatch.setenv("GITHUB_USERNAME", "u")
    monkeypatch.setenv("CLAUDE_MODEL", "Opus")

    c = Config.load()
    assert c.claude_model == "opus"
