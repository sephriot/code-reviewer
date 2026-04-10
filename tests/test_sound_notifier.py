import pytest

from code_reviewer.sound_notifier import SoundNotifier


@pytest.mark.asyncio
async def test_play_notification_applies_template_context(monkeypatch):
    spoken = {}

    async def fake_play_tts(self, text=""):
        spoken["text"] = text

    async def fake_play_system(self):
        spoken["system"] = True

    monkeypatch.setattr(SoundNotifier, "_play_tts", fake_play_tts)
    monkeypatch.setattr(SoundNotifier, "_play_system_sound", fake_play_system)

    notifier = SoundNotifier(sound_file="say:Review {title} in {repo} by {author}")

    await notifier.play_notification(
        {
            "repo": "acme/widgets",
            "pr_number": 42,
            "author": "octocat",
            "title": "Fix race condition",
        }
    )

    assert spoken["text"] == "Review Fix race condition in acme/widgets by octocat"
    assert "system" not in spoken


@pytest.mark.asyncio
async def test_play_all_enabled_uses_demo_context_for_templates(monkeypatch):
    spoken = []

    async def fake_play_tts(self, text=""):
        spoken.append(text)

    async def fake_play_system(self):
        spoken.append("system")

    monkeypatch.setattr(SoundNotifier, "_play_tts", fake_play_tts)
    monkeypatch.setattr(SoundNotifier, "_play_system_sound", fake_play_system)

    notifier = SoundNotifier(
        sound_file="say:Review {title} in {repo} by {author} #{pr_number}",
        approval_sound_enabled=False,
        human_review_sound_enabled=False,
        timeout_sound_enabled=False,
        merged_or_closed_sound_enabled=False,
        own_pr_ready_sound_enabled=False,
        own_pr_needs_attention_sound_enabled=False,
        review_started_sound_enabled=False,
    )

    await notifier.play_all_enabled()

    assert spoken == [
        "Review Demo pull request in demo/repository by demo-author #123"
    ]


@pytest.mark.asyncio
async def test_play_merged_or_closed_sound_uses_custom_file(tmp_path, monkeypatch):
    played = {}

    async def fake_play_sound(self, file_path):
        played['path'] = file_path

    async def fake_play_system(self):
        played['system'] = True

    monkeypatch.setattr(SoundNotifier, '_play_sound_file', fake_play_sound)
    monkeypatch.setattr(SoundNotifier, '_play_system_sound', fake_play_system)

    sound_path = tmp_path / 'merged_or_closed.wav'
    sound_path.write_bytes(b'')

    notifier = SoundNotifier(merged_or_closed_sound_file=sound_path)
    await notifier.play_merged_or_closed_sound()

    assert played['path'] == sound_path
    assert 'system' not in played


@pytest.mark.asyncio
async def test_play_merged_or_closed_sound_skips_when_disabled(monkeypatch):
    called = {}

    async def fake_play_system(self):
        called['system'] = True

    monkeypatch.setattr(SoundNotifier, '_play_system_sound', fake_play_system)

    notifier = SoundNotifier(merged_or_closed_sound_enabled=False)
    await notifier.play_merged_or_closed_sound()

    assert 'system' not in called


@pytest.mark.asyncio
async def test_play_human_review_sound_skips_when_disabled(monkeypatch):
    called = []

    async def fake_play_system(self):
        called.append("system")

    monkeypatch.setattr(SoundNotifier, "_play_system_sound", fake_play_system)

    notifier = SoundNotifier(human_review_sound_enabled=False)
    await notifier.play_human_review_sound()

    assert called == []


@pytest.mark.asyncio
async def test_runtime_mute_skips_all_playback(monkeypatch):
    called = []

    async def fake_play_system(self):
        called.append("system")

    monkeypatch.setattr(SoundNotifier, "_play_system_sound", fake_play_system)

    notifier = SoundNotifier()
    notifier.set_runtime_mute_all(True)

    await notifier.play_notification()
    await notifier.play_approval_sound()
    await notifier.play_human_review_sound()

    assert called == []


@pytest.mark.asyncio
async def test_runtime_mute_cleared_allows_playback(monkeypatch):
    called = []

    async def fake_play_system(self):
        called.append("system")

    monkeypatch.setattr(SoundNotifier, "_play_system_sound", fake_play_system)

    notifier = SoundNotifier(
        approval_sound_enabled=False,
        human_review_sound_enabled=False,
    )
    notifier.set_runtime_mute_all(True)
    notifier.set_runtime_mute_all(False)

    await notifier.play_notification()

    assert called == ["system"]
