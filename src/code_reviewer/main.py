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
from .claude_integration import ClaudeIntegration
from .github_client import GitHubClient
from .web_server import ReviewWebServer


class CodeReviewer:
    def __init__(self, config: Config):
        self.config = config
        self.running = True
        self.github_client = GitHubClient(config.github_token)
        self.claude_integration = ClaudeIntegration(config.claude_prompt_file)
        self.monitor = GitHubMonitor(
            self.github_client, 
            self.claude_integration, 
            config
        )
        self.web_server = None
        if config.web_enabled:
            self.web_server = ReviewWebServer(self.monitor.db, self.github_client)
        
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
        click.echo(f"Using prompt file: {self.config.claude_prompt_file}")
        
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
        await server.serve()


@click.command()
@click.option('--config', '-c', type=click.Path(exists=True), 
              help='Path to configuration file')
@click.option('--prompt', '-p', type=click.Path(exists=True), 
              help='Path to Claude prompt file')
@click.option('--github-token', envvar='GITHUB_TOKEN', 
              help='GitHub personal access token')
@click.option('--github-username', envvar='GITHUB_USERNAME', 
              help='GitHub username to monitor')
@click.option('--poll-interval', default=60, type=int,
              help='Polling interval in seconds (default: 60)')
@click.option('--sound-enabled/--no-sound', default=True,
              help='Enable/disable sound notifications (default: enabled)')
@click.option('--sound-file', type=click.Path(exists=True),
              help='Custom sound file for notifications')
@click.option('--dry-run', is_flag=True, default=False,
              help='Log what actions would be taken without actually performing them')
@click.option('--web-enabled/--no-web', default=False,
              help='Enable/disable web UI for managing approvals (default: disabled)')
@click.option('--web-host', default='127.0.0.1',
              help='Host for web UI server (default: 127.0.0.1)')
@click.option('--web-port', default=8000, type=int,
              help='Port for web UI server (default: 8000)')
def main(config: Optional[str], prompt: Optional[str], github_token: Optional[str], 
         github_username: Optional[str], poll_interval: int, sound_enabled: bool,
         sound_file: Optional[str], dry_run: bool, web_enabled: bool,
         web_host: str, web_port: int):
    """Automated GitHub PR code review using Claude."""
    
    load_dotenv()
    
    try:
        app_config = Config.load(
            config_file=config,
            claude_prompt_file=prompt,
            github_token=github_token,
            github_username=github_username,
            poll_interval=poll_interval,
            sound_enabled=sound_enabled,
            sound_file=sound_file,
            dry_run=dry_run,
            web_enabled=web_enabled,
            web_host=web_host,
            web_port=web_port
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