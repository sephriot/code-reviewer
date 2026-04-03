#!/usr/bin/env python3
"""Main entry point for the code reviewer application."""

import asyncio
import json
import signal
import sys
from pathlib import Path
from typing import List, Optional

import click
from dotenv import load_dotenv
import uvicorn

from .config import Config
from .github_monitor import GitHubMonitor
from .llm_integration import LLMIntegration
from .github_client import GitHubClient
from .web_server import ReviewWebServer
from .models import ReviewModel


class CodeReviewer:
    def __init__(self, config: Config):
        self.config = config
        self.running = True
        self.github_client = GitHubClient(config.github_token)
        self.model_integration = LLMIntegration(
            config.prompt_file,
            config.review_model,
            output_format_file=config.output_format_file,
            show_thinking=config.show_thinking,
            atlas_enabled=config.atlas_enabled,
            agent_argv=config.review_agent_argv,
        )
        self.monitor = GitHubMonitor(self.github_client, self.model_integration, config)
        self.web_server = None
        if config.web_enabled:
            self.web_server = ReviewWebServer(
                self.monitor.db,
                self.github_client,
                self.monitor.sound_notifier,
                self.model_integration,
            )

    def signal_handler(self, signum, frame):
        """Handle SIGTERM gracefully."""
        click.echo("Received SIGTERM, shutting down gracefully...")
        self.running = False
        self.monitor.running = False

    async def run(self):
        """Main application loop."""
        signal.signal(signal.SIGTERM, self.signal_handler)
        signal.signal(signal.SIGINT, self.signal_handler)

        click.echo("Starting GitHub PR monitor...")
        click.echo(f"Monitoring user: {self.config.github_username}")
        click.echo(f"Using prompt file: {self.config.prompt_file}")
        if self.config.output_format_file:
            click.echo(f"Using output format file: {self.config.output_format_file}")
        click.echo(f"Using review model: {self.config.review_model.value}")
        click.echo(
            f"Atlas knowledge injection: {'enabled' if self.config.atlas_enabled else 'disabled'}"
        )

        if self.config.web_enabled:
            click.echo(
                f"Web UI enabled at http://{self.config.web_host}:{self.config.web_port}"
            )

        templates = self.monitor.sound_notifier.get_available_templates()
        click.echo("TTS templates available: " + ", ".join(t[0] for t in templates))
        for placeholder, desc in templates:
            click.echo(f"  {placeholder} - {desc}")

        await self.monitor.sound_notifier.play_all_enabled(
            self.monitor.sound_notifier.get_demo_context()
        )

        try:
            if self.config.web_enabled:
                if self.config.own_pr_enabled:
                    await asyncio.gather(
                        self.monitor.start_monitoring(),
                        self.monitor.start_own_prs_monitoring(),
                        self._run_web_server(),
                    )
                else:
                    await asyncio.gather(
                        self.monitor.start_monitoring(), self._run_web_server()
                    )
            else:
                if self.config.own_pr_enabled:
                    await asyncio.gather(
                        self.monitor.start_monitoring(),
                        self.monitor.start_own_prs_monitoring(),
                    )
                else:
                    await self.monitor.start_monitoring()
        except KeyboardInterrupt:
            click.echo("Interrupted by user")
        except Exception as e:
            click.echo(f"Error: {e}", err=True)
            sys.exit(1)
        finally:
            try:
                if hasattr(self.monitor, "cleanup_sync"):
                    click.echo("Starting cleanup...")
                    self.monitor.cleanup_sync()
                    click.echo("Cleanup completed")
                else:
                    click.echo("Monitor does not have cleanup method")
            except Exception as e:
                click.echo(f"Error during cleanup: {e}")
                import traceback

                traceback.print_exc()
            click.echo("Shutting down...")

    async def _run_web_server(self):
        """Run the web server in asyncio event loop."""
        config = uvicorn.Config(
            self.web_server.get_app(),
            host=self.config.web_host,
            port=self.config.web_port,
            log_level="info",
        )
        server = uvicorn.Server(config)

        # Add a startup message
        click.echo(
            f"🌐 Web dashboard is running at: http://{self.config.web_host}:{self.config.web_port}"
        )
        click.echo(
            "   Visit the dashboard to manage pending approvals and human reviews"
        )

        await server.serve()


@click.command()
@click.option(
    "--config", "-c", type=click.Path(exists=True), help="Path to configuration file"
)
@click.option(
    "--prompt", "-p", type=click.Path(exists=True), help="Path to prompt template file"
)
@click.option(
    "--output-format",
    "-o",
    "output_format",
    type=click.Path(exists=True),
    help="Path to output format prompt file (appended to the review prompt)",
)
@click.option(
    "--model",
    "review_model",
    type=click.Choice(["CLAUDE", "CODEX", "AGENT"]),
    envvar="REVIEW_MODEL",
    default="CLAUDE",
    help="Language model CLI to use for reviews (env: REVIEW_MODEL)",
)
@click.option(
    "--review-agent-argv",
    "review_agent_argv_json",
    type=str,
    default=None,
    envvar="REVIEW_AGENT_ARGV",
    help=(
        "When --model AGENT: JSON array of argv for the Cursor `agent` CLI "
        '(default: ["agent","--print","--output-format","json","--trust"]). '
        "Env: REVIEW_AGENT_ARGV"
    ),
)
@click.option(
    "--github-token", envvar="GITHUB_TOKEN", help="GitHub personal access token"
)
@click.option(
    "--github-username", envvar="GITHUB_USERNAME", help="GitHub username to monitor"
)
@click.option(
    "--poll-interval",
    default=60,
    type=int,
    help="Polling interval in seconds (default: 60)",
)
@click.option(
    "--review-timeout",
    envvar="REVIEW_TIMEOUT",
    default=None,
    type=int,
    help="Maximum seconds to allow an automated review to run before timing out (env: REVIEW_TIMEOUT, 0 disables)",
)
@click.option(
    "--sound-enabled/--no-sound",
    default=True,
    help="Enable/disable sound notifications (default: enabled)",
)
@click.option(
    "--sound-file",
    type=str,
    help="Custom sound file for notifications (supports 'say:text' for TTS, 'path/to/file' for audio files)",
)
@click.option(
    "--approval-sound-enabled/--no-approval-sound",
    envvar="APPROVAL_SOUND_ENABLED",
    default=True,
    help="Enable/disable sound notifications for PR approvals (default: enabled, env: APPROVAL_SOUND_ENABLED)",
)
@click.option(
    "--approval-sound-file",
    envvar="APPROVAL_SOUND_FILE",
    type=str,
    help="Custom sound file for PR approval notifications (supports 'say:text' for TTS, 'path/to/file' for audio files)",
)
@click.option(
    "--timeout-sound-enabled/--no-timeout-sound",
    envvar="TIMEOUT_SOUND_ENABLED",
    default=None,
    help="Enable/disable sound notifications for review timeouts (env: TIMEOUT_SOUND_ENABLED)",
)
@click.option(
    "--timeout-sound-file",
    envvar="TIMEOUT_SOUND_FILE",
    type=str,
    help="Custom sound file for review timeout notifications (supports 'say:text' for TTS, 'path/to/file' for audio files)",
)
@click.option(
    "--outdated-sound-enabled/--no-outdated-sound",
    envvar="OUTDATED_SOUND_ENABLED",
    default=None,
    help="Enable/disable sound notifications when pending approvals become merged or closed (env: MERGED_OR_CLOSED_SOUND_ENABLED or OUTDATED_SOUND_ENABLED)",
)
@click.option(
    "--outdated-sound-file",
    envvar="OUTDATED_SOUND_FILE",
    type=str,
    help="Custom sound file for merged/closed pending approval notifications (env: MERGED_OR_CLOSED_SOUND_FILE or OUTDATED_SOUND_FILE)",
)
@click.option(
    "--own-pr-enabled/--no-own-pr",
    envvar="OWN_PR_ENABLED",
    default=False,
    help="Enable monitoring of your own PRs (default: disabled, env: OWN_PR_ENABLED)",
)
@click.option(
    "--own-pr-ready-sound-enabled/--no-own-pr-ready-sound",
    envvar="OWN_PR_READY_SOUND_ENABLED",
    default=True,
    help="Enable/disable sound notifications when own PR is ready for merging (default: enabled, env: OWN_PR_READY_SOUND_ENABLED)",
)
@click.option(
    "--own-pr-ready-sound-file",
    envvar="OWN_PR_READY_SOUND_FILE",
    type=str,
    help="Custom sound file for own PR ready notifications (env: OWN_PR_READY_SOUND_FILE)",
)
@click.option(
    "--own-pr-needs-attention-sound-enabled/--no-own-pr-needs-attention-sound",
    envvar="OWN_PR_NEEDS_ATTENTION_SOUND_ENABLED",
    default=True,
    help="Enable/disable sound notifications when own PR needs attention (default: enabled, env: OWN_PR_NEEDS_ATTENTION_SOUND_ENABLED)",
)
@click.option(
    "--own-pr-needs-attention-sound-file",
    envvar="OWN_PR_NEEDS_ATTENTION_SOUND_FILE",
    type=str,
    help="Custom sound file for own PR needs attention notifications (env: OWN_PR_NEEDS_ATTENTION_SOUND_FILE)",
)
@click.option(
    "--dry-run",
    is_flag=True,
    default=False,
    help="Log what actions would be taken without actually performing them",
)
@click.option(
    "--web-enabled/--no-web",
    envvar="WEB_ENABLED",
    default=False,
    help="Enable/disable web UI for managing approvals (default: disabled, env: WEB_ENABLED)",
)
@click.option(
    "--web-host",
    envvar="WEB_HOST",
    default="127.0.0.1",
    help="Host for web UI server (default: 127.0.0.1, env: WEB_HOST)",
)
@click.option(
    "--web-port",
    envvar="WEB_PORT",
    default=8000,
    type=int,
    help="Port for web UI server (default: 8000, env: WEB_PORT)",
)
@click.option(
    "--show-thinking/--no-show-thinking",
    envvar="SHOW_THINKING",
    default=False,
    help="Show Claude thinking process in logs (default: disabled, env: SHOW_THINKING)",
)
@click.option(
    "--atlas-enabled/--no-atlas",
    envvar="ATLAS_ENABLED",
    default=False,
    help="Enable Atlas knowledge injection for reviews (default: disabled, env: ATLAS_ENABLED)",
)
def main(
    config: Optional[str],
    prompt: Optional[str],
    output_format: Optional[str],
    review_model: str,
    review_agent_argv_json: Optional[str],
    github_token: Optional[str],
    github_username: Optional[str],
    poll_interval: int,
    review_timeout: Optional[int],
    sound_enabled: bool,
    sound_file: Optional[str],
    approval_sound_enabled: bool,
    approval_sound_file: Optional[str],
    timeout_sound_enabled: Optional[bool],
    timeout_sound_file: Optional[str],
    outdated_sound_enabled: Optional[bool],
    outdated_sound_file: Optional[str],
    own_pr_enabled: bool,
    own_pr_ready_sound_enabled: bool,
    own_pr_ready_sound_file: Optional[str],
    own_pr_needs_attention_sound_enabled: bool,
    own_pr_needs_attention_sound_file: Optional[str],
    dry_run: bool,
    web_enabled: bool,
    web_host: str,
    web_port: int,
    show_thinking: bool,
    atlas_enabled: bool,
):
    """Automated GitHub PR code review using Claude."""

    load_dotenv()

    try:
        review_agent_argv: Optional[List[str]] = None
        if review_agent_argv_json:
            try:
                review_agent_argv = json.loads(review_agent_argv_json)
            except json.JSONDecodeError as exc:
                raise ValueError(
                    "--review-agent-argv / REVIEW_AGENT_ARGV must be a JSON array of strings: "
                    f"{exc}"
                ) from exc
            if not isinstance(review_agent_argv, list) or not all(
                isinstance(x, str) for x in review_agent_argv
            ):
                raise ValueError(
                    "--review-agent-argv / REVIEW_AGENT_ARGV must be a JSON array of strings"
                )
            if len(review_agent_argv) == 0:
                raise ValueError(
                    "--review-agent-argv / REVIEW_AGENT_ARGV cannot be an empty array"
                )

        load_kwargs = dict(
            config_file=config,
            prompt_file=prompt,
            output_format_file=output_format,
            review_model=review_model,
            github_token=github_token,
            github_username=github_username,
            poll_interval=poll_interval,
            review_timeout=review_timeout,
            sound_enabled=sound_enabled,
            sound_file=sound_file,
            approval_sound_enabled=approval_sound_enabled,
            approval_sound_file=approval_sound_file,
            timeout_sound_enabled=timeout_sound_enabled,
            timeout_sound_file=timeout_sound_file,
            merged_or_closed_sound_enabled=outdated_sound_enabled,
            merged_or_closed_sound_file=outdated_sound_file,
            own_pr_enabled=own_pr_enabled,
            own_pr_ready_sound_enabled=own_pr_ready_sound_enabled,
            own_pr_ready_sound_file=own_pr_ready_sound_file,
            own_pr_needs_attention_sound_enabled=own_pr_needs_attention_sound_enabled,
            own_pr_needs_attention_sound_file=own_pr_needs_attention_sound_file,
            dry_run=dry_run,
            web_enabled=web_enabled,
            web_host=web_host,
            web_port=web_port,
            show_thinking=show_thinking,
            atlas_enabled=atlas_enabled,
        )
        if review_agent_argv is not None:
            load_kwargs["review_agent_argv"] = review_agent_argv

        app_config = Config.load(**load_kwargs)

        # Set up logging
        app_config.setup_logging()

        reviewer = CodeReviewer(app_config)
        asyncio.run(reviewer.run())

    except Exception as e:
        click.echo(f"Failed to start application: {e}", err=True)
        sys.exit(1)


if __name__ == "__main__":
    main()
