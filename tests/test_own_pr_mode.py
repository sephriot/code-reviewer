"""Tests for own PR mode configuration (OWN_PR_MODE / legacy OWN_PR_ENABLED)."""

import pytest

from code_reviewer.config import Config
from code_reviewer.models import OwnPRMode


@pytest.fixture
def base_env(monkeypatch):
    monkeypatch.setenv("GITHUB_TOKEN", "t")
    monkeypatch.setenv("GITHUB_USERNAME", "u")
    monkeypatch.delenv("OWN_PR_MODE", raising=False)
    monkeypatch.delenv("OWN_PR_ENABLED", raising=False)
    return monkeypatch


def test_default_mode_is_off(base_env):
    assert Config.load().own_pr_mode == OwnPRMode.OFF


def test_mode_from_env_is_case_insensitive(base_env):
    base_env.setenv("OWN_PR_MODE", "Manual")
    assert Config.load().own_pr_mode == OwnPRMode.MANUAL


def test_legacy_enabled_true_maps_to_auto(base_env):
    base_env.setenv("OWN_PR_ENABLED", "true")
    assert Config.load().own_pr_mode == OwnPRMode.AUTO


def test_legacy_enabled_false_maps_to_off(base_env):
    base_env.setenv("OWN_PR_ENABLED", "false")
    assert Config.load().own_pr_mode == OwnPRMode.OFF


def test_explicit_mode_wins_over_legacy_enabled(base_env):
    base_env.setenv("OWN_PR_ENABLED", "true")
    base_env.setenv("OWN_PR_MODE", "off")
    assert Config.load().own_pr_mode == OwnPRMode.OFF


def test_cli_mode_override_wins_over_legacy_env(base_env):
    base_env.setenv("OWN_PR_ENABLED", "true")
    assert Config.load(own_pr_mode="manual").own_pr_mode == OwnPRMode.MANUAL


def test_legacy_cli_override_maps_to_auto(base_env):
    assert Config.load(own_pr_enabled=True).own_pr_mode == OwnPRMode.AUTO


def test_invalid_mode_raises(base_env):
    base_env.setenv("OWN_PR_MODE", "sometimes")
    with pytest.raises(ValueError, match="Unsupported own PR mode"):
        Config.load()
