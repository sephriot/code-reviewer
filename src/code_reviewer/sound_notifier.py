"""Sound notification functionality."""

import asyncio
import logging
import platform
import subprocess
from pathlib import Path
from typing import Optional, Union


logger = logging.getLogger(__name__)


class SoundNotifier:
    """Cross-platform sound notification system."""

    DEMO_CONTEXT = {
        "repo": "demo/repository",
        "pr_number": 123,
        "author": "demo-author",
        "title": "Demo pull request",
    }

    def __init__(
        self,
        enabled: bool = True,
        sound_file: Optional[Union["SoundFileConfig", Path]] = None,
        approval_sound_enabled: bool = True,
        approval_sound_file: Optional[Union["SoundFileConfig", Path]] = None,
        human_review_sound_enabled: bool = True,
        human_review_sound_file: Optional[Union["SoundFileConfig", Path]] = None,
        timeout_sound_enabled: bool = True,
        timeout_sound_file: Optional[Union["SoundFileConfig", Path]] = None,
        merged_or_closed_sound_enabled: bool = True,
        merged_or_closed_sound_file: Optional[Union["SoundFileConfig", Path]] = None,
        own_pr_ready_sound_enabled: bool = True,
        own_pr_ready_sound_file: Optional[Union["SoundFileConfig", Path]] = None,
        own_pr_needs_attention_sound_enabled: bool = True,
        own_pr_needs_attention_sound_file: Optional[
            Union["SoundFileConfig", Path]
        ] = None,
        review_started_sound_enabled: bool = True,
        review_started_sound_file: Optional[Union["SoundFileConfig", Path]] = None,
        *,
        outdated_sound_enabled: Optional[bool] = None,
        outdated_sound_file: Optional[Union["SoundFileConfig", Path]] = None,
    ):
        from .config import SoundFileConfig

        self.enabled = enabled
        self.sound_file = self._normalize_sound_file(sound_file)
        self.approval_sound_enabled = approval_sound_enabled
        self.approval_sound_file = self._normalize_sound_file(approval_sound_file)
        self.human_review_sound_enabled = human_review_sound_enabled
        self.human_review_sound_file = self._normalize_sound_file(
            human_review_sound_file
        )
        self.timeout_sound_enabled = timeout_sound_enabled
        self.timeout_sound_file = self._normalize_sound_file(timeout_sound_file)
        if outdated_sound_enabled is not None:
            merged_or_closed_sound_enabled = outdated_sound_enabled
        if outdated_sound_file is not None:
            merged_or_closed_sound_file = self._normalize_sound_file(
                outdated_sound_file
            )

        self.merged_or_closed_sound_enabled = merged_or_closed_sound_enabled
        self.merged_or_closed_sound_file = self._normalize_sound_file(
            merged_or_closed_sound_file
        )
        self.own_pr_ready_sound_enabled = own_pr_ready_sound_enabled
        self.own_pr_ready_sound_file = self._normalize_sound_file(
            own_pr_ready_sound_file
        )
        self.own_pr_needs_attention_sound_enabled = own_pr_needs_attention_sound_enabled
        self.own_pr_needs_attention_sound_file = self._normalize_sound_file(
            own_pr_needs_attention_sound_file
        )
        self.review_started_sound_enabled = review_started_sound_enabled
        self.review_started_sound_file = self._normalize_sound_file(
            review_started_sound_file
        )
        self.system = platform.system().lower()

    def _normalize_sound_file(self, value):
        """Normalize sound file config to SoundFileConfig."""
        from .config import SoundFileConfig, Config

        if value is None:
            return None
        if isinstance(value, SoundFileConfig):
            return value
        if isinstance(value, Path):
            return SoundFileConfig(path=value)
        if isinstance(value, str):
            return Config._parse_sound_file(value)
        return value

    async def _play_sound_config(
        self, sound_config, default_text: str = "", context: dict = None
    ):
        """Play a sound based on SoundFileConfig - handles both file and TTS."""
        from .config import SoundFileConfig

        if sound_config is None:
            await self._play_system_sound()
            return

        # Apply template if context provided
        if context and sound_config.text:
            sound_config = sound_config.apply_template(context)

        # Check if it's a TTS config (tool:text format)
        if sound_config.is_tts():
            await self._play_tts(sound_config.text or default_text)
        # Check if it's a file path that exists
        elif sound_config.is_file():
            await self._play_sound_file(sound_config.path)
        # No valid config, fall back to system sound
        else:
            await self._play_system_sound()

    @classmethod
    def get_demo_context(cls) -> dict:
        """Return synthetic PR metadata for startup/demo playback."""
        return dict(cls.DEMO_CONTEXT)

    async def play_notification(self, context: dict = None):
        """Play a notification sound."""
        if not self.enabled:
            logger.debug("Sound notifications are disabled")
            return

        try:
            await self._play_sound_config(self.sound_file, "Notification", context)
        except Exception as e:
            logger.warning(f"Failed to play notification sound: {e}")

    async def play_approval_sound(self, context: dict = None):
        """Play a sound when PR is approved."""
        if not self.approval_sound_enabled:
            logger.debug("Approval sound notifications are disabled")
            return

        try:
            await self._play_sound_config(self.approval_sound_file, "Approved", context)
        except Exception as e:
            logger.warning(f"Failed to play approval sound: {e}")

    async def play_human_review_sound(self, context: dict = None):
        """Play a sound when a PR is marked as requiring human review."""
        if not self.human_review_sound_enabled:
            logger.debug("Human review sound notifications are disabled")
            return

        try:
            await self._play_sound_config(
                self.human_review_sound_file,
                "PR requires human review",
                context,
            )
        except Exception as e:
            logger.warning(f"Failed to play human review sound: {e}")

    async def play_timeout_sound(self, context: dict = None):
        """Play a sound when an automated review times out."""
        if not self.timeout_sound_enabled:
            logger.debug("Timeout sound notifications are disabled")
            return

        try:
            await self._play_sound_config(self.timeout_sound_file, "Timeout", context)
        except Exception as e:
            logger.warning(f"Failed to play timeout sound: {e}")

    async def play_merged_or_closed_sound(self, context: dict = None):
        """Play a sound when pending approvals become merged_or_closed."""
        if not self.merged_or_closed_sound_enabled:
            logger.debug("Merged/closed sound notifications are disabled")
            return

        try:
            await self._play_sound_config(
                self.merged_or_closed_sound_file, "Outdated", context
            )
        except Exception as e:
            logger.warning(f"Failed to play merged/closed sound: {e}")

    async def play_outdated_sound(self, context: dict = None):
        """Backward compatible alias for play_merged_or_closed_sound."""
        logger.debug(
            "play_outdated_sound is deprecated; forwarding to play_merged_or_closed_sound"
        )
        await self.play_merged_or_closed_sound(context)

    async def play_pr_ready_sound(self, context: dict = None):
        """Play a sound when own PR is ready for merging."""
        if not self.own_pr_ready_sound_enabled:
            logger.debug("Own PR ready sound notifications are disabled")
            return

        try:
            await self._play_sound_config(
                self.own_pr_ready_sound_file, "Ready for merge", context
            )
        except Exception as e:
            logger.warning(f"Failed to play own PR ready sound: {e}")

    async def play_pr_needs_attention_sound(self, context: dict = None):
        """Play a sound when own PR needs attention."""
        if not self.own_pr_needs_attention_sound_enabled:
            logger.debug("Own PR needs attention sound notifications are disabled")
            return

        try:
            await self._play_sound_config(
                self.own_pr_needs_attention_sound_file, "Needs attention", context
            )
        except Exception as e:
            logger.warning(f"Failed to play own PR needs attention sound: {e}")

    async def play_all_enabled(self, context: dict = None):
        """Play all enabled sounds (used for startup notification)."""
        playback_context = context or self.get_demo_context()
        await self.play_notification(playback_context)
        await self.play_approval_sound(playback_context)
        await self.play_human_review_sound(playback_context)
        await self.play_timeout_sound(playback_context)
        await self.play_merged_or_closed_sound(playback_context)
        await self.play_pr_ready_sound(playback_context)
        await self.play_pr_needs_attention_sound(playback_context)
        await self.play_review_started_sound(playback_context)

    async def play_review_started_sound(self, context: dict = None):
        """Play a sound when review process starts for a PR."""
        if not self.review_started_sound_enabled:
            logger.debug("Review started sound notifications are disabled")
            return

        try:
            await self._play_sound_config(
                self.review_started_sound_file, "Review started", context
            )
        except Exception as e:
            logger.warning(f"Failed to play review started sound: {e}")

    async def _play_tts(self, text: str = ""):
        """Play a text-to-speech notification."""
        if not text:
            text = "Notification"

        try:
            if self.system == "darwin":
                cmd = ["say", text]
            elif self.system == "linux":
                for tts in ["espeak", "festival"]:
                    if await self._command_exists(tts):
                        if tts == "espeak":
                            cmd = ["espeak", text]
                        else:
                            cmd = ["festival", "--tts"]
                        break
                else:
                    logger.warning("No TTS tool found on Linux")
                    return
            elif self.system == "windows":
                cmd = [
                    "powershell",
                    "-c",
                    f"Add-Type -AssemblyName System.Speech; $synth = New-Object System.Speech.Synthesis.SpeechSynthesizer; $synth.Speak('{text}')",
                ]
            else:
                logger.warning(f"TTS not supported on {self.system}")
                return

            process = await asyncio.create_subprocess_exec(
                *cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
            )
            await process.communicate()

        except Exception as e:
            logger.warning(f"Failed to play TTS sound: {e}")

    async def _play_custom_sound(self):
        """Play a custom sound file."""
        if not self.enabled:
            logger.debug("Sound notifications are disabled; skipping custom sound")
            return

        try:
            if self.sound_file and self.sound_file.exists():
                await self._play_sound_file(self.sound_file)
            else:
                await self._play_system_sound()
        except Exception as e:
            logger.warning(f"Failed to play custom sound: {e}")
            await self._play_system_sound()

    async def _play_sound_file(self, file_path: Path):
        """Play a specific sound file."""
        if not file_path:
            raise ValueError("Sound file path is not set")

        if self.system == "darwin":  # macOS
            cmd = ["afplay", str(file_path)]
        elif self.system == "linux":
            for player in ["aplay", "paplay", "sox"]:
                if await self._command_exists(player):
                    if player == "sox":
                        cmd = ["play", str(file_path)]
                    else:
                        cmd = [player, str(file_path)]
                    break
            else:
                raise RuntimeError("No audio player found")
        elif self.system == "windows":
            cmd = [
                "powershell",
                "-c",
                f"(New-Object Media.SoundPlayer '{file_path}').PlaySync()",
            ]
        else:
            raise RuntimeError(f"Unsupported system: {self.system}")

        process = await asyncio.create_subprocess_exec(
            *cmd,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        await process.communicate()

    async def _play_system_sound(self):
        """Play a system notification sound."""
        try:
            if self.system == "darwin":  # macOS
                # Play system alert sound
                cmd = ["osascript", "-e", "beep"]
            elif self.system == "linux":
                # Try different methods to play system beep
                for method in [
                    [
                        "pactl",
                        "upload-sample",
                        "/usr/share/sounds/alsa/Noise.wav",
                        "beep",
                    ],
                    ["speaker-test", "-t", "sine", "-f", "1000", "-l", "1"],
                    ["echo", "-e", "\\a"],  # Terminal bell
                ]:
                    try:
                        process = await asyncio.create_subprocess_exec(
                            *method,
                            stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL,
                        )
                        result = await process.communicate()
                        if process.returncode == 0:
                            break
                    except:
                        continue
                else:
                    # Final fallback - print bell character
                    print("\\a", end="", flush=True)
                    return
            elif self.system == "windows":
                # Use Windows system beep
                cmd = ["powershell", "-c", "[console]::beep(1000,500)"]
            else:
                # Generic fallback
                print("\\a", end="", flush=True)
                return

            process = await asyncio.create_subprocess_exec(
                *cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
            )
            await process.communicate()

        except Exception as e:
            logger.warning(f"Failed to play system sound: {e}")
            # Ultimate fallback - just print bell
            print("\\a", end="", flush=True)

    async def _command_exists(self, command: str) -> bool:
        """Check if a command exists in the system PATH."""
        try:
            process = await asyncio.create_subprocess_exec(
                "which" if self.system != "windows" else "where",
                command,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            await process.communicate()
            return process.returncode == 0
        except:
            return False

    def create_default_sound_file(self, output_path: Path):
        """Create a simple beep sound file if none exists."""
        try:
            if self.system == "darwin":
                # Generate a simple beep using sox (if available)
                cmd = ["sox", "-n", str(output_path), "synth", "0.5", "sine", "1000"]
            elif self.system == "linux":
                # Generate using sox or other tools
                cmd = ["sox", "-n", str(output_path), "synth", "0.5", "sine", "1000"]
            else:
                logger.warning("Cannot create default sound file on this system")
                return

            subprocess.run(cmd, check=True, capture_output=True)
            logger.info(f"Created default sound file: {output_path}")

        except (subprocess.CalledProcessError, FileNotFoundError) as e:
            logger.warning(f"Failed to create default sound file: {e}")

    @staticmethod
    def get_available_templates() -> list[tuple]:
        """Return list of available template placeholders for TTS sounds."""
        from .config import SOUND_TEMPLATE_PLACEHOLDERS

        return SOUND_TEMPLATE_PLACEHOLDERS
