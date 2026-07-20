"""Tests for Cursor Agent (AGENT) review model integration."""

import asyncio
import io
import json
from pathlib import Path

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
    p = Path("prompts/review_prompt.txt")
    integration = LLMIntegration(p, ReviewModel.AGENT, agent_argv=None)
    cmd = integration._build_command()
    assert cmd == list(LLMIntegration._DEFAULT_CURSOR_AGENT_ARGV)


def test_custom_agent_argv():
    custom = ["agent", "--print", "--output-format", "text"]
    integration = LLMIntegration(
        Path("prompts/review_prompt.txt"), ReviewModel.AGENT, agent_argv=custom
    )
    assert integration._build_command() == custom


def test_show_thinking_enables_agent_stream_json_output():
    integration = LLMIntegration(
        Path("prompts/review_prompt.txt"), ReviewModel.AGENT, show_thinking=True
    )

    assert integration._build_command() == [
        "agent",
        "--print",
        "--output-format",
        "stream-json",
        "--trust",
    ]


def test_show_thinking_replaces_custom_agent_output_format():
    integration = LLMIntegration(
        Path("prompts/review_prompt.txt"),
        ReviewModel.AGENT,
        show_thinking=True,
        agent_argv=["agent", "--print", "--output-format", "text"],
    )

    assert integration._build_command() == [
        "agent",
        "--print",
        "--output-format",
        "stream-json",
    ]


def test_show_thinking_removes_custom_partial_output_flag():
    integration = LLMIntegration(
        Path("prompts/review_prompt.txt"),
        ReviewModel.AGENT,
        show_thinking=True,
        agent_argv=[
            "agent",
            "--print",
            "--output-format",
            "stream-json",
            "--stream-partial-output",
        ],
    )

    assert "--stream-partial-output" not in integration._build_command()


@pytest.mark.asyncio
async def test_stream_cursor_agent_output_displays_events_and_returns_result():
    stream = asyncio.StreamReader()
    events = [
        {"type": "assistant", "message": {"content": [{"type": "text", "text": "Looking"}]}},
        {"type": "tool_call", "subtype": "started", "tool_call": {"name": "Shell"}},
        {
            "type": "result",
            "subtype": "success",
            "result": '{"action": "approve_without_comment"}',
        },
    ]
    stream.feed_data("".join(f"{json.dumps(event)}\n" for event in events).encode())
    stream.feed_eof()
    output = io.StringIO()
    buffer: list[str] = []

    result = await LLMIntegration._stream_cursor_agent_output(
        stream, buffer, output_stream=output
    )

    assert result == '{"action": "approve_without_comment"}'
    assert output.getvalue() == "".join(f"{json.dumps(event)}\n" for event in events)
    assert "\"type\": \"tool_call\"" in "".join(buffer)


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
