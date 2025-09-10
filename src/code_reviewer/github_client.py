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
            # Don't add any filters to the query - filter on application side instead
            query = f"type:pr state:open review-requested:{username}"
            
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
                        # Get author and title information from the API response
                        author = item.get('user', {}).get('login', '')
                        title = item.get('title', '')
                        repository = item['repository_url'].split('/')[-2:]  # [owner, repo]
                        
                        # Fetch detailed PR information including head/base SHAs
                        detailed_pr = await self._fetch_pr_details(repository[0], repository[1], item['number'])
                        if not detailed_pr:
                            logger.warning(f"Failed to fetch details for PR #{item['number']} in {'/'.join(repository)}")
                            continue
                            
                        # Include title, author, and SHA information in PRInfo
                        pr_info = PRInfo(
                            id=item['id'],
                            number=item['number'],
                            repository=repository,
                            url=item['html_url'],
                            title=title,
                            author=author,
                            head_sha=detailed_pr.get('head', {}).get('sha', ''),
                            base_sha=detailed_pr.get('base', {}).get('sha', '')
                        )
                        all_prs.append(pr_info)
                
                # Apply filters on the application side
                logger.debug(f"Found {len(all_prs)} total PRs before filtering")
                for pr in all_prs:
                    logger.debug(f"  PR #{pr.number} in {pr.repository_name} by {pr.author}: {pr.title}")
                
                filtered_prs = all_prs
                
                # Filter by repositories
                if repositories:
                    logger.info(f"Filtering PRs to repositories: {', '.join(repositories)}")
                    logger.debug(f"Available repositories: {[pr.repository_name for pr in filtered_prs]}")
                    filtered_prs = [pr for pr in filtered_prs if pr.repository_name in repositories]
                
                # Filter by PR authors
                if pr_authors:
                    logger.info(f"Filtering PRs to authors: {', '.join(pr_authors)}")
                    filtered_prs = [pr for pr in filtered_prs if pr.author in pr_authors]
                
                if repositories or pr_authors:
                    logger.debug(f"Found {len(all_prs)} total PRs, {len(filtered_prs)} match filters")
                else:
                    logger.info("No filters specified, monitoring all accessible PRs")
                        
                return filtered_prs
                
        except Exception as e:
            logger.error(f"Error fetching review requests: {e}")
            return []

    async def _fetch_pr_details(self, owner: str, repo: str, pr_number: int) -> Optional[dict]:
        """Fetch detailed PR information including head and base SHAs."""
        try:
            if not self.session:
                self.session = aiohttp.ClientSession(
                    headers={
                        'Authorization': f'token {self.token}',
                        'Accept': 'application/vnd.github.v3+json',
                    }
                )
            
            url = f"https://api.github.com/repos/{owner}/{repo}/pulls/{pr_number}"
            async with self.session.get(url) as response:
                if response.status == 200:
                    return await response.json()
                else:
                    logger.error(f"Failed to fetch PR details: {response.status}")
                    return None
                    
        except Exception as e:
            logger.error(f"Error fetching PR details for #{pr_number} in {owner}/{repo}: {e}")
            return None
            
            
    async def approve_pr(self, repository: List[str], pr_number: int, comment: Optional[str] = None, 
                       inline_comments: Optional[List[Dict[str, Any]]] = None):
        """Approve a PR with optional comment and inline comments."""
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
                
            # Add inline comments if provided
            if inline_comments:
                data['comments'] = []
                for comment_data in inline_comments:
                    inline_comment = {
                        'path': comment_data.get('path', comment_data.get('file')),
                        'line': comment_data.get('line'),
                        'body': comment_data.get('body', comment_data.get('message'))
                    }
                    data['comments'].append(inline_comment)
                logger.info(f"Added {len(data['comments'])} inline comments to approval")
                for i, comment in enumerate(data['comments']):
                    logger.info(f"  Comment {i}: {comment['path']}:{comment['line']} - {comment['body'][:100]}...")
                
            logger.info(f"Posting approval to GitHub with data: {data}")
            async with self.session.post(url, json=data) as response:
                result = await response.json()
                
                if response.status not in [200, 201]:
                    raise Exception(f"GitHub API error: {result}")
                    
                logger.info(f"Successfully approved PR #{pr_number}")
                return True
                
        except Exception as e:
            logger.error(f"Error approving PR: {e}")
            return False
            
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
                        'path': comment.get('path', comment.get('file')),
                        'line': comment.get('line'),
                        'body': comment.get('body', comment.get('message'))
                    }
                    data['comments'].append(inline_comment)
                    
            async with self.session.post(url, json=data) as response:
                result = await response.json()
                
                if response.status not in [200, 201]:
                    raise Exception(f"GitHub API error: {result}")
                    
                logger.info(f"Successfully requested changes for PR #{pr_number}")
                return True
                
        except Exception as e:
            logger.error(f"Error requesting changes: {e}")
            return False
            
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
                return True
                
        except Exception as e:
            logger.error(f"Error adding review comment: {e}")
            return False