"""GitHub API client for PR operations."""

import asyncio
import logging
from typing import List, Dict, Any, Optional

import aiohttp
from github import Github, PullRequest


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
            
    async def get_review_requests(self, username: str, repositories: Optional[List[str]] = None) -> List[Dict[str, Any]]:
        """Get PRs where the user is requested as a reviewer."""
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
                
                prs = []
                for item in data.get('items', []):
                    if item.get('pull_request'):  # Ensure it's a PR
                        pr_info = {
                            'id': item['id'],
                            'number': item['number'],
                            'title': item['title'],
                            'repository': item['repository_url'].split('/')[-2:],  # owner/repo
                            'author': item['user']['login'],
                            'url': item['html_url']
                        }
                        prs.append(pr_info)
                        
                return prs
                
        except Exception as e:
            logger.error(f"Error fetching review requests: {e}")
            return []
            
    async def get_pr_details(self, repository: List[str], pr_number: int) -> Dict[str, Any]:
        """Get detailed information about a PR including files and diff."""
        try:
            owner, repo_name = repository[0], repository[1]
            
            if not self.session:
                self.session = aiohttp.ClientSession(
                    headers={
                        'Authorization': f'token {self.token}',
                        'Accept': 'application/vnd.github.v3+json',
                    }
                )
            
            # Get PR details
            pr_url = f"https://api.github.com/repos/{owner}/{repo_name}/pulls/{pr_number}"
            async with self.session.get(pr_url) as response:
                pr_data = await response.json()
                
                if response.status != 200:
                    raise Exception(f"GitHub API error: {pr_data}")
            
            # Get PR files
            files_url = f"https://api.github.com/repos/{owner}/{repo_name}/pulls/{pr_number}/files"
            async with self.session.get(files_url) as response:
                files_data = await response.json()
                
                if response.status != 200:
                    raise Exception(f"GitHub API error: {files_data}")
            
            # Process files and get content for new/modified files
            files = []
            for file_info in files_data:
                file_data = {
                    'filename': file_info['filename'],
                    'status': file_info['status'],
                    'additions': file_info['additions'],
                    'deletions': file_info['deletions'],
                    'patch': file_info.get('patch', '')
                }
                
                # Get full file content for new/modified files
                if file_info['status'] in ['added', 'modified']:
                    content = await self._get_file_content(owner, repo_name, file_info['filename'], pr_data['head']['sha'])
                    if content:
                        file_data['content'] = content
                        
                files.append(file_data)
            
            return {
                'title': pr_data['title'],
                'body': pr_data.get('body', ''),
                'author': pr_data['user']['login'],
                'repository': f"{owner}/{repo_name}",
                'number': pr_number,
                'head_branch': pr_data['head']['ref'],
                'base_branch': pr_data['base']['ref'],
                'changed_files': [f['filename'] for f in files],
                'additions': pr_data['additions'],
                'deletions': pr_data['deletions'],
                'files': files,
                'head_sha': pr_data['head']['sha'],
                'base_sha': pr_data['base']['sha'],
                'updated_at': pr_data['updated_at']
            }
            
        except Exception as e:
            logger.error(f"Error getting PR details: {e}")
            raise
            
    async def _get_file_content(self, owner: str, repo: str, file_path: str, sha: str) -> Optional[str]:
        """Get the content of a file at a specific commit."""
        try:
            url = f"https://api.github.com/repos/{owner}/{repo}/contents/{file_path}"
            params = {'ref': sha}
            
            async with self.session.get(url, params=params) as response:
                if response.status == 200:
                    data = await response.json()
                    import base64
                    return base64.b64decode(data['content']).decode('utf-8')
                else:
                    logger.warning(f"Could not get content for {file_path}: {response.status}")
                    return None
                    
        except Exception as e:
            logger.error(f"Error getting file content for {file_path}: {e}")
            return None
            
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