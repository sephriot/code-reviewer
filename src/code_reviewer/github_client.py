"""GitHub API client for PR operations."""

import asyncio
import logging
import re
from typing import List, Dict, Any, Optional, Set, Tuple

import aiohttp
from github import Github

from .models import PRInfo, InlineComment


logger = logging.getLogger(__name__)


class GitHubClient:
    def __init__(self, token: str):
        self.token = token
        self.github = Github(token)
        self.session = None
        self._default_headers = {
            'Authorization': f'token {self.token}',
            'Accept': 'application/vnd.github.v3+json',
        }

    def _ensure_session(self) -> None:
        """Ensure an aiohttp session is available."""
        if self.session is None or self.session.closed:
            self.session = aiohttp.ClientSession(headers=self._default_headers)
        
    async def __aenter__(self):
        self._ensure_session()
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
            
            self._ensure_session()
            
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
            self._ensure_session()
            
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

    async def prepare_inline_comments(
        self,
        repository: List[str],
        pr_number: int,
        inline_comments: List[InlineComment],
    ) -> Tuple[List[Dict[str, Any]], List[InlineComment]]:
        """Validate inline comments against the PR diff and return GitHub-ready payload."""
        if not inline_comments:
            return [], []

        owner, repo_name = repository[0], repository[1]
        valid_lines_map = await self._collect_valid_comment_lines(owner, repo_name, pr_number)

        valid_payload: List[Dict[str, Any]] = []
        dropped: List[InlineComment] = []

        for comment in inline_comments:
            path = comment.file
            body = comment.message

            try:
                line = int(comment.line)
            except (TypeError, ValueError):
                logger.warning(
                    "Skipping inline comment for %s#%s: invalid line '%s'",
                    f"{owner}/{repo_name}",
                    pr_number,
                    comment.line,
                )
                dropped.append(comment)
                continue

            if not path:
                logger.warning(
                    "Skipping inline comment for %s#%s: missing file path",
                    f"{owner}/{repo_name}",
                    pr_number,
                )
                dropped.append(comment)
                continue

            valid_lines = valid_lines_map.get(path)
            if valid_lines and line in valid_lines:
                valid_payload.append({
                    'path': path,
                    'line': line,
                    'side': 'RIGHT',
                    'body': body,
                })
            else:
                # Log diagnostic info about valid line range for debugging LLM line number issues
                if valid_lines:
                    min_line = min(valid_lines)
                    max_line = max(valid_lines)
                    logger.warning(
                        "Dropping inline comment for %s#%s at %s:%s - line not in diff "
                        "(valid range for this file: %d-%d, %d valid lines)",
                        f"{owner}/{repo_name}",
                        pr_number,
                        path,
                        line,
                        min_line,
                        max_line,
                        len(valid_lines),
                    )
                else:
                    logger.warning(
                        "Dropping inline comment for %s#%s at %s:%s - file not found in diff",
                        f"{owner}/{repo_name}",
                        pr_number,
                        path,
                        line,
                    )
                dropped.append(comment)

        return valid_payload, dropped

    async def _collect_valid_comment_lines(
        self, owner: str, repo: str, pr_number: int
    ) -> Dict[str, Set[int]]:
        """Collect the set of valid new-file line numbers per file for inline comments."""
        try:
            patches = await self._fetch_pr_file_patches(owner, repo, pr_number)
        except Exception as exc:  # pragma: no cover - network failure fallback
            logger.error(
                "Failed to load PR diff for %s/%s#%s: %s",
                owner,
                repo,
                pr_number,
                exc,
            )
            return {}

        line_map: Dict[str, Set[int]] = {}
        for filename, patch in patches.items():
            if not patch:
                continue
            line_map[filename] = self._extract_valid_diff_lines(patch)

        return line_map

    async def _fetch_pr_file_patches(
        self, owner: str, repo: str, pr_number: int
    ) -> Dict[str, str]:
        """Fetch unified diff patches for each file in the PR."""
        self._ensure_session()

        patches: Dict[str, str] = {}
        page = 1
        url = f"https://api.github.com/repos/{owner}/{repo}/pulls/{pr_number}/files"

        while True:
            params = {'per_page': 100, 'page': page}
            async with self.session.get(url, params=params) as response:
                data = await response.json()

                if response.status != 200:
                    raise Exception(f"GitHub API error: {data}")

                for file_info in data:
                    filename = file_info.get('filename')
                    patch = file_info.get('patch')
                    if filename and patch:
                        patches[filename] = patch

                link_header = response.headers.get('Link', '')
                if link_header and 'rel="next"' in link_header:
                    page += 1
                    continue

                if len(data) == 100:
                    page += 1
                    continue

                break

        return patches

    @staticmethod
    def _extract_valid_diff_lines(patch: str) -> Set[int]:
        """Parse a unified diff patch and return valid new-file line numbers."""
        valid_lines: Set[int] = set()
        new_line_number: Optional[int] = None

        for raw_line in patch.splitlines():
            if raw_line.startswith('@@'):
                match = re.search(r"\+([0-9]+)(?:,([0-9]+))?", raw_line)
                if match:
                    new_line_number = int(match.group(1))
                else:
                    new_line_number = None
                continue

            if new_line_number is None:
                continue

            if raw_line.startswith('+') or raw_line.startswith(' '):
                valid_lines.add(new_line_number)
                new_line_number += 1
            elif raw_line.startswith('-'):
                # Removed lines don't advance the new file line counter
                continue
            else:
                # Metadata lines (e.g. "\\ No newline at end of file")
                continue

        return valid_lines

    @staticmethod
    def format_dropped_inline_comments(dropped: List[InlineComment]) -> str:
        """Build fallback text describing inline comments we couldn't post."""
        if not dropped:
            return ''

        lines = [
            "The following suggestions could not be attached inline because the referenced lines are outside the latest diff:",
        ]

        for comment in dropped:
            location = comment.file or 'unknown file'
            if comment.line:
                location = f"{location}:{comment.line}"

            message = (comment.message or '').strip().replace('\n', ' ')
            if len(message) > 300:
                message = f"{message[:297]}..."

            lines.append(f"- `{location}` – {message}")

        return "\n\n" + "\n".join(lines)
            
            
    async def get_pr_status(self, repository: Any, pr_number: int) -> Optional[dict]:
        """Fetch current state/merge information for a PR."""
        try:
            if isinstance(repository, (list, tuple)):
                owner, repo_name = repository
            elif isinstance(repository, str) and '/' in repository:
                owner, repo_name = repository.split('/', 1)
            else:
                raise ValueError(f"Invalid repository identifier: {repository}")

            pr_data = await self._fetch_pr_details(owner, repo_name, pr_number)
            if not pr_data:
                return None

            return {
                'state': pr_data.get('state'),
                'merged': pr_data.get('merged'),
                'head_sha': pr_data.get('head', {}).get('sha'),
                'base_sha': pr_data.get('base', {}).get('sha'),
                'updated_at': pr_data.get('updated_at'),
                'closed_at': pr_data.get('closed_at'),
                'merged_at': pr_data.get('merged_at'),
            }

        except Exception as e:
            logger.error(
                f"Error fetching PR status for #{pr_number} in {repository}: {e}"
            )
            return None


    async def approve_pr(self, repository: List[str], pr_number: int, comment: Optional[str] = None, 
                       inline_comments: Optional[List[Dict[str, Any]]] = None):
        """Approve a PR with optional comment and inline comments."""
        try:
            owner, repo_name = repository[0], repository[1]
            
            self._ensure_session()
            
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
                        'body': comment_data.get('body', comment_data.get('message')),
                    }

                    line_value = comment_data.get('line')
                    if line_value is not None:
                        try:
                            inline_comment['line'] = int(line_value)
                        except (ValueError, TypeError):
                            logger.warning(
                                "Skipping non-integer line value for inline comment: %s",
                                line_value,
                            )
                            continue

                    start_line = comment_data.get('start_line')
                    if start_line is not None:
                        try:
                            inline_comment['start_line'] = int(start_line)
                        except (ValueError, TypeError):
                            logger.warning(
                                "Skipping non-integer start_line for inline comment: %s",
                                start_line,
                            )

                    side = comment_data.get('side')
                    if side:
                        inline_comment['side'] = side
                    elif 'line' in inline_comment:
                        inline_comment['side'] = 'RIGHT'

                    start_side = comment_data.get('start_side')
                    if start_side:
                        inline_comment['start_side'] = start_side

                    position = comment_data.get('position')
                    if position is not None:
                        inline_comment['position'] = position

                    data['comments'].append(inline_comment)
                logger.info(f"Added {len(data['comments'])} inline comments to approval")
                for i, comment in enumerate(data['comments']):
                    line_desc = comment.get('line', comment.get('position', 'n/a'))
                    logger.info(f"  Comment {i}: {comment['path']}:{line_desc} - {comment['body'][:100]}...")
                
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
            
            self._ensure_session()
            
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
                        'body': comment.get('body', comment.get('message')),
                    }

                    line_value = comment.get('line')
                    if line_value is not None:
                        try:
                            inline_comment['line'] = int(line_value)
                        except (ValueError, TypeError):
                            logger.warning(
                                "Skipping non-integer line value for inline comment: %s",
                                line_value,
                            )
                            continue

                    start_line = comment.get('start_line')
                    if start_line is not None:
                        try:
                            inline_comment['start_line'] = int(start_line)
                        except (ValueError, TypeError):
                            logger.warning(
                                "Skipping non-integer start_line for inline comment: %s",
                                start_line,
                            )

                    side = comment.get('side')
                    if side:
                        inline_comment['side'] = side
                    elif 'line' in inline_comment:
                        inline_comment['side'] = 'RIGHT'

                    start_side = comment.get('start_side')
                    if start_side:
                        inline_comment['start_side'] = start_side

                    position = comment.get('position')
                    if position is not None:
                        inline_comment['position'] = position

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
            
            self._ensure_session()
            
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
