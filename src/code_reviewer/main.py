#!/usr/bin/env python3
"""Main entry point for the code reviewer application."""

import asyncio
import signal
import sys
from pathlib import Path
from typing import Optional

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
        self.model_integration = LLMIntegration(config.prompt_file, config.review_model, show_thinking=config.show_thinking, atlas_enabled=config.atlas_enabled)
        self.monitor = GitHubMonitor(
            self.github_client,
            self.model_integration,
            config
        )
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
        click.echo(f"Using review model: {self.config.review_model.value}")
        click.echo(f"Atlas knowledge injection: {'enabled' if self.config.atlas_enabled else 'disabled'}")

        if self.config.web_enabled:
            click.echo(f"Web UI enabled at http://{self.config.web_host}:{self.config.web_port}")
        
        try:
            if self.config.web_enabled:
                # Run both the monitor and web server concurrently
                await asyncio.gather(
                    self.monitor.start_monitoring(),
                    self._run_web_server()
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
                if hasattr(self.monitor, 'cleanup_sync'):
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
            log_level="info"
        )
        server = uvicorn.Server(config)
        
        # Add a startup message
        click.echo(f"🌐 Web dashboard is running at: http://{self.config.web_host}:{self.config.web_port}")
        click.echo("   Visit the dashboard to manage pending approvals and human reviews")
        
        await server.serve()


@click.command()
@click.option('--config', '-c', type=click.Path(exists=True), 
              help='Path to configuration file')
@click.option('--prompt', '-p', type=click.Path(exists=True), 
              help='Path to prompt template file')
@click.option('--model', 'review_model', type=click.Choice(['CLAUDE', 'CODEX']),
             envvar='REVIEW_MODEL', default='CLAUDE',
             help='Language model CLI to use for reviews (env: REVIEW_MODEL)')
@click.option('--github-token', envvar='GITHUB_TOKEN', 
              help='GitHub personal access token')
@click.option('--github-username', envvar='GITHUB_USERNAME', 
              help='GitHub username to monitor')
@click.option('--poll-interval', default=60, type=int,
              help='Polling interval in seconds (default: 60)')
@click.option('--review-timeout', envvar='REVIEW_TIMEOUT', default=None, type=int,
              help='Maximum seconds to allow an automated review to run before timing out (env: REVIEW_TIMEOUT, 0 disables)')
@click.option('--sound-enabled/--no-sound', default=True,
              help='Enable/disable sound notifications (default: enabled)')
@click.option('--sound-file', type=click.Path(exists=True),
              help='Custom sound file for notifications')
@click.option('--approval-sound-enabled/--no-approval-sound', envvar='APPROVAL_SOUND_ENABLED', default=True,
              help='Enable/disable sound notifications for PR approvals (default: enabled, env: APPROVAL_SOUND_ENABLED)')
@click.option('--approval-sound-file', envvar='APPROVAL_SOUND_FILE', type=click.Path(exists=True),
              help='Custom sound file for PR approval notifications (env: APPROVAL_SOUND_FILE)')
@click.option('--timeout-sound-enabled/--no-timeout-sound', envvar='TIMEOUT_SOUND_ENABLED', default=None,
              help='Enable/disable sound notifications for review timeouts (env: TIMEOUT_SOUND_ENABLED)')
@click.option('--timeout-sound-file', envvar='TIMEOUT_SOUND_FILE', type=click.Path(exists=True),
              help='Custom sound file for review timeout notifications (env: TIMEOUT_SOUND_FILE)')
@click.option('--outdated-sound-enabled/--no-outdated-sound', envvar='OUTDATED_SOUND_ENABLED', default=None,
              help='Enable/disable sound notifications when pending approvals become merged or closed (env: MERGED_OR_CLOSED_SOUND_ENABLED or OUTDATED_SOUND_ENABLED)')
@click.option('--outdated-sound-file', envvar='OUTDATED_SOUND_FILE', type=click.Path(exists=True),
              help='Custom sound file for merged/closed pending approval notifications (env: MERGED_OR_CLOSED_SOUND_FILE or OUTDATED_SOUND_FILE)')
@click.option('--dry-run', is_flag=True, default=False,
              help='Log what actions would be taken without actually performing them')
@click.option('--web-enabled/--no-web', envvar='WEB_ENABLED', default=False,
              help='Enable/disable web UI for managing approvals (default: disabled, env: WEB_ENABLED)')
@click.option('--web-host', envvar='WEB_HOST', default='127.0.0.1',
              help='Host for web UI server (default: 127.0.0.1, env: WEB_HOST)')
@click.option('--web-port', envvar='WEB_PORT', default=8000, type=int,
              help='Port for web UI server (default: 8000, env: WEB_PORT)')
@click.option('--show-thinking/--no-show-thinking', envvar='SHOW_THINKING', default=False,
              help='Show Claude thinking process in logs (default: disabled, env: SHOW_THINKING)')
@click.option('--atlas-enabled/--no-atlas', envvar='ATLAS_ENABLED', default=False,
              help='Enable Atlas knowledge injection for reviews (default: disabled, env: ATLAS_ENABLED)')
def main(config: Optional[str], prompt: Optional[str], review_model: str, github_token: Optional[str],
         github_username: Optional[str], poll_interval: int, review_timeout: Optional[int], sound_enabled: bool,
         sound_file: Optional[str], approval_sound_enabled: bool,
         approval_sound_file: Optional[str], timeout_sound_enabled: Optional[bool],
         timeout_sound_file: Optional[str], outdated_sound_enabled: Optional[bool],
         outdated_sound_file: Optional[str], dry_run: bool, web_enabled: bool,
         web_host: str, web_port: int, show_thinking: bool, atlas_enabled: bool):
    """Automated GitHub PR code review using Claude."""
    
    load_dotenv()
    
    try:
        app_config = Config.load(
            config_file=config,
            prompt_file=prompt,
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
            dry_run=dry_run,
            web_enabled=web_enabled,
            web_host=web_host,
            web_port=web_port,
            show_thinking=show_thinking,
            atlas_enabled=atlas_enabled
        )
        
        # Set up logging
        app_config.setup_logging()
        
        reviewer = CodeReviewer(app_config)
        asyncio.run(reviewer.run())
        
    except Exception as e:
        click.echo(f"Failed to start application: {e}", err=True)
        sys.exit(1)


if __name__ == '__main__':
    main()
