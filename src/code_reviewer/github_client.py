"""GitHub API client for PR operations."""

import asyncio
import logging
from typing import List, Dict, Any, Optional

import aiohttp
from github import Github, PullRequest

from .models import PRInfo


logger = logging.getLogger(__name__)


class GitHubClient:
    def __init__(self, token: str):
        self.token = token
        self.github = Github(token)
        self.session = None
        
    async def __aenter__(self):
        self.session = aiohttp.ClientSession(
            headers={
                'Authorization': f'token {self.token}',
                'Accept': 'application/vnd.github.v3+json',
            }
        )
        return self
        
    async def __aexit__(self, exc_type, exc_val, exc_tb):
        if self.session:
            await self.session.close()
            
    async def close(self):
        """Close the aiohttp session."""
        logger.debug(f"GitHubClient.close() called, session: {self.session}")
        if self.session and not self.session.closed:
            logger.debug("Closing aiohttp session...")
            await self.session.close()
            self.session = None
            logger.debug("aiohttp session closed successfully")
        elif self.session:
            logger.debug("aiohttp session already closed")
            self.session = None
        else:
            logger.debug("No session to close")
            
    async def get_review_requests(self, username: str, repositories: Optional[List[str]] = None, pr_authors: Optional[List[str]] = None) -> List[PRInfo]:
        """Get PRs where the user is requested as a reviewer - minimal info only."""
        try:
            # Use GitHub search API to find PRs where user is requested as reviewer
            query = f"type:pr state:open review-requested:{username}"
            
            # Add repository filtering if specified
            if repositories:
                repo_filters = []
                for repo in repositories:
                    # Handle both "owner/repo" and "repo" formats
                    if '/' in repo:
                        repo_filters.append(f"repo:{repo}")
                    else:
                        # If just repo name, we can't filter without owner
                        logger.warning(f"Repository '{repo}' should be in 'owner/repo' format for filtering")
                        continue
                
                if repo_filters:
                    # Add repository filters to query
                    repo_query = " OR ".join(repo_filters)
                    query += f" ({repo_query})"
                    logger.info(f"Filtering PRs to repositories: {', '.join(repositories)}")
                else:
                    logger.warning("No valid repositories found for filtering, monitoring all repositories")
            
            if not self.session:
                self.session = aiohttp.ClientSession(
                    headers={
                        'Authorization': f'token {self.token}',
                        'Accept': 'application/vnd.github.v3+json',
                    }
                )
            
            url = f"https://api.github.com/search/issues"
            params = {
                'q': query,
                'sort': 'updated',
                'order': 'desc'
            }
            
            async with self.session.get(url, params=params) as response:
                data = await response.json()
                
                if response.status != 200:
                    raise Exception(f"GitHub API error: {data}")
                
                all_prs = []
                for item in data.get('items', []):
                    if item.get('pull_request'):  # Ensure it's a PR
                        # Get author information from the API response
                        author = item.get('user', {}).get('login', '')
                        
                        # Only return minimal info - Claude Code will fetch the rest
                        pr_info = PRInfo(
                            id=item['id'],
                            number=item['number'],
                            repository=item['repository_url'].split('/')[-2:],  # [owner, repo]
                            url=item['html_url']
                        )
                        # Store author for filtering (not part of PRInfo dataclass)
                        pr_info._author = author
                        all_prs.append(pr_info)
                
                # Apply filters on the application side
                filtered_prs = all_prs
                
                # Filter by repositories
                if repositories:
                    logger.info(f"Filtering PRs to repositories: {', '.join(repositories)}")
                    filtered_prs = [pr for pr in filtered_prs if pr.repository_name in repositories]
                
                # Filter by PR authors
                if pr_authors:
                    logger.info(f"Filtering PRs to authors: {', '.join(pr_authors)}")
                    filtered_prs = [pr for pr in filtered_prs if hasattr(pr, '_author') and pr._author in pr_authors]
                
                if repositories or pr_authors:
                    logger.debug(f"Found {len(all_prs)} total PRs, {len(filtered_prs)} match filters")
                else:
                    logger.info("No filters specified, monitoring all accessible PRs")
                
                # Clean up temporary author attribute
                for pr in filtered_prs:
                    if hasattr(pr, '_author'):
                        delattr(pr, '_author')
                        
                return filtered_prs
                
        except Exception as e:
            logger.error(f"Error fetching review requests: {e}")
            return []
            
            
    async def approve_pr(self, repository: List[str], pr_number: int, comment: Optional[str] = None):
        """Approve a PR with optional comment."""
        try:
            owner, repo_name = repository[0], repository[1]
            
            if not self.session:
                self.session = aiohttp.ClientSession(
                    headers={
                        'Authorization': f'token {self.token}',
                        'Accept': 'application/vnd.github.v3+json',
                    }
                )
            
            url = f"https://api.github.com/repos/{owner}/{repo_name}/pulls/{pr_number}/reviews"
            
            data = {
                'event': 'APPROVE'
            }
            
            if comment:
                data['body'] = comment
                
            async with self.session.post(url, json=data) as response:
                result = await response.json()
                
                if response.status not in [200, 201]:
                    raise Exception(f"GitHub API error: {result}")
                    
                logger.info(f"Successfully approved PR #{pr_number}")
                
        except Exception as e:
            logger.error(f"Error approving PR: {e}")
            raise
            
    async def request_changes(self, repository: List[str], pr_number: int, 
                           comments: List[Dict[str, Any]], summary: str):
        """Request changes on a PR with inline comments."""
        try:
            owner, repo_name = repository[0], repository[1]
            
            if not self.session:
                self.session = aiohttp.ClientSession(
                    headers={
                        'Authorization': f'token {self.token}',
                        'Accept': 'application/vnd.github.v3+json',
                    }
                )
            
            url = f"https://api.github.com/repos/{owner}/{repo_name}/pulls/{pr_number}/reviews"
            
            data = {
                'event': 'REQUEST_CHANGES',
                'body': summary
            }
            
            # Add inline comments if provided
            if comments:
                data['comments'] = []
                for comment in comments:
                    inline_comment = {
                        'path': comment['file'],
                        'line': comment['line'],
                        'body': comment['message']
                    }
                    data['comments'].append(inline_comment)
                    
            async with self.session.post(url, json=data) as response:
                result = await response.json()
                
                if response.status not in [200, 201]:
                    raise Exception(f"GitHub API error: {result}")
                    
                logger.info(f"Successfully requested changes for PR #{pr_number}")
                
        except Exception as e:
            logger.error(f"Error requesting changes: {e}")
            raise
            
    async def add_review_comment(self, repository: List[str], pr_number: int, comment: str):
        """Add a general comment to a PR without approving or requesting changes."""
        try:
            owner, repo_name = repository[0], repository[1]
            
            if not self.session:
                self.session = aiohttp.ClientSession(
                    headers={
                        'Authorization': f'token {self.token}',
                        'Accept': 'application/vnd.github.v3+json',
                    }
                )
            
            url = f"https://api.github.com/repos/{owner}/{repo_name}/pulls/{pr_number}/reviews"
            
            data = {
                'event': 'COMMENT',
                'body': comment
            }
                    
            async with self.session.post(url, json=data) as response:
                result = await response.json()
                
                if response.status not in [200, 201]:
                    raise Exception(f"GitHub API error: {result}")
                    
                logger.info(f"Successfully added comment to PR #{pr_number}")
                
        except Exception as e:
            logger.error(f"Error adding review comment: {e}")
            raise