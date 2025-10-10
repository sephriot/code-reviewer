import pytest

from code_reviewer.sound_notifier import SoundNotifier


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
