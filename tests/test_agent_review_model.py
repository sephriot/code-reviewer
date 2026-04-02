"""Tests for Cursor Agent (AGENT) review model integration."""

import json

import pytest

from code_reviewer.llm_integration import LLMIntegration
from code_reviewer.models import ReviewModel


def test_unwrap_cursor_agent_json_envelope():
    envelope = {
        "type": "result",
        "subtype": "success",
        "result": '{"action": "approve_without_comment"}',
    }
    raw = json.dumps(envelope)
    out = LLMIntegration._unwrap_cursor_agent_json(raw)
    assert out == '{"action": "approve_without_comment"}'


def test_unwrap_cursor_agent_json_passes_through_plain_json():
    inner = '{"action": "approve_without_comment"}'
    assert LLMIntegration._unwrap_cursor_agent_json(inner) == inner


def test_default_agent_argv():
    from pathlib import Path

    p = Path("prompts/review_prompt.txt")
    integration = LLMIntegration(p, ReviewModel.AGENT, agent_argv=None)
    cmd = integration._build_command()
    assert cmd == list(LLMIntegration._DEFAULT_CURSOR_AGENT_ARGV)


def test_custom_agent_argv():
    from pathlib import Path

    custom = ["agent", "--print", "--output-format", "text"]
    integration = LLMIntegration(
        Path("prompts/review_prompt.txt"), ReviewModel.AGENT, agent_argv=custom
    )
    assert integration._build_command() == custom


def test_review_agent_argv_config_json_env(monkeypatch, tmp_path):
    monkeypatch.setenv("GITHUB_TOKEN", "t")
    monkeypatch.setenv("GITHUB_USERNAME", "u")
    monkeypatch.setenv(
        "REVIEW_AGENT_ARGV",
        json.dumps(["agent", "--print", "--output-format", "json"]),
    )
    from code_reviewer.config import Config

    c = Config.load()
    assert c.review_agent_argv == ["agent", "--print", "--output-format", "json"]


def test_review_agent_argv_invalid_raises(monkeypatch, tmp_path):
    monkeypatch.setenv("GITHUB_TOKEN", "t")
    monkeypatch.setenv("GITHUB_USERNAME", "u")
    monkeypatch.setenv("REVIEW_AGENT_ARGV", "not-json")
    from code_reviewer.config import Config

    with pytest.raises(ValueError, match="REVIEW_AGENT_ARGV"):
        Config.load()
