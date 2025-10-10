"""Sound notification functionality."""

import asyncio
import logging
import platform
import subprocess
from pathlib import Path
from typing import Optional


logger = logging.getLogger(__name__)


class SoundNotifier:
    """Cross-platform sound notification system."""
    
    def __init__(
        self,
        enabled: bool = True,
        sound_file: Optional[Path] = None,
        approval_sound_enabled: bool = True,
        approval_sound_file: Optional[Path] = None,
        timeout_sound_enabled: bool = True,
        timeout_sound_file: Optional[Path] = None,
        outdated_sound_enabled: bool = True,
        outdated_sound_file: Optional[Path] = None,
    ):
        self.enabled = enabled
        self.sound_file = sound_file
        self.approval_sound_enabled = approval_sound_enabled
        self.approval_sound_file = approval_sound_file
        self.timeout_sound_enabled = timeout_sound_enabled
        self.timeout_sound_file = timeout_sound_file
        self.outdated_sound_enabled = outdated_sound_enabled
        self.outdated_sound_file = outdated_sound_file
        self.system = platform.system().lower()
        
    async def play_notification(self):
        """Play a notification sound."""
        if not self.enabled:
            logger.debug("Sound notifications are disabled")
            return
            
        try:
            if self.sound_file and self.sound_file.exists():
                await self._play_custom_sound()
            else:
                await self._play_system_sound()
                
        except Exception as e:
            logger.warning(f"Failed to play notification sound: {e}")

    async def play_approval_sound(self):
        """Play a sound when PR is approved."""
        if not self.approval_sound_enabled:
            logger.debug("Approval sound notifications are disabled")
            return
            
        try:
            if self.approval_sound_file and self.approval_sound_file.exists():
                await self._play_sound_file(self.approval_sound_file)
            elif self.enabled and self.sound_file and self.sound_file.exists():
                # Fallback to general notification sound
                await self._play_custom_sound()
            else:
                await self._play_system_sound()
                
        except Exception as e:
            logger.warning(f"Failed to play approval sound: {e}")
    async def play_timeout_sound(self):
        """Play a sound when an automated review times out."""
        if not self.timeout_sound_enabled:
            logger.debug("Timeout sound notifications are disabled")
            return

        try:
            if self.timeout_sound_file and self.timeout_sound_file.exists():
                await self._play_sound_file(self.timeout_sound_file)
            elif self.enabled and self.sound_file and self.sound_file.exists():
                await self._play_custom_sound()
            else:
                await self._play_system_sound()
        except Exception as e:
            logger.warning(f"Failed to play timeout sound: {e}")
            await self._play_system_sound()

    async def play_outdated_sound(self):
        """Play a sound when pending approvals become outdated."""
        if not self.outdated_sound_enabled:
            logger.debug("Outdated sound notifications are disabled")
            return

        try:
            if self.outdated_sound_file and self.outdated_sound_file.exists():
                await self._play_sound_file(self.outdated_sound_file)
            elif self.enabled and self.sound_file and self.sound_file.exists():
                await self._play_custom_sound()
            else:
                await self._play_system_sound()
        except Exception as e:
            logger.warning(f"Failed to play outdated sound: {e}")
            await self._play_system_sound()

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
                    ["pactl", "upload-sample", "/usr/share/sounds/alsa/Noise.wav", "beep"],
                    ["speaker-test", "-t", "sine", "-f", "1000", "-l", "1"],
                    ["echo", "-e", "\\a"]  # Terminal bell
                ]:
                    try:
                        process = await asyncio.create_subprocess_exec(
                            *method,
                            stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL
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
                *cmd,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL
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
                stderr=subprocess.DEVNULL
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
                cmd = [
                    "sox", "-n", str(output_path), 
                    "synth", "0.5", "sine", "1000"
                ]
            elif self.system == "linux":
                # Generate using sox or other tools
                cmd = [
                    "sox", "-n", str(output_path), 
                    "synth", "0.5", "sine", "1000"
                ]
            else:
                logger.warning("Cannot create default sound file on this system")
                return
                
            subprocess.run(cmd, check=True, capture_output=True)
            logger.info(f"Created default sound file: {output_path}")
            
        except (subprocess.CalledProcessError, FileNotFoundError) as e:
            logger.warning(f"Failed to create default sound file: {e}")
