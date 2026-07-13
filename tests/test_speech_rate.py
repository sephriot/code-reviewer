import asyncio
from pathlib import Path

import pytest

from code_reviewer.config import Config
from code_reviewer.sound_notifier import SoundNotifier


def test_config_defaults_speech_rate(monkeypatch):
    monkeypatch.setenv("GITHUB_TOKEN", "t")
    monkeypatch.setenv("GITHUB_USERNAME", "u")

    config = Config.load()

    assert config.speech_rate == 200


def test_config_loads_speech_rate_from_env(monkeypatch):
    monkeypatch.setenv("GITHUB_TOKEN", "t")
    monkeypatch.setenv("GITHUB_USERNAME", "u")
    monkeypatch.setenv("SPEECH_RATE", "175")

    config = Config.load()

    assert config.speech_rate == 175


def test_config_loads_speech_rate_from_config_file(tmp_path):
    config_file = tmp_path / "config.yaml"
    config_file.write_text(
        "\n".join(
            [
                'github_token: "t"',
                'github_username: "u"',
                "speech_rate: 240",
            ]
        ),
        encoding="utf-8",
    )

    config = Config.load(config_file=str(config_file))

    assert config.speech_rate == 240


@pytest.mark.parametrize("value", ["0", "-1", "fast"])
def test_config_rejects_invalid_speech_rate(monkeypatch, value):
    monkeypatch.setenv("GITHUB_TOKEN", "t")
    monkeypatch.setenv("GITHUB_USERNAME", "u")
    monkeypatch.setenv("SPEECH_RATE", value)

    with pytest.raises(ValueError, match="speech_rate"):
        Config.load()


@pytest.mark.asyncio
async def test_macos_say_uses_configured_speech_rate(monkeypatch):
    command = []

    class FakeProcess:
        returncode = 0

        async def communicate(self):
            return None

    async def fake_create_subprocess_exec(*args, **kwargs):
        command.extend(args)
        return FakeProcess()

    monkeypatch.setattr(
        asyncio,
        "create_subprocess_exec",
        fake_create_subprocess_exec,
    )

    notifier = SoundNotifier(speech_rate=175)
    notifier.system = "darwin"

    await notifier._play_tts("Review started")

    assert command == ["say", "-r", "175", "Review started"]
