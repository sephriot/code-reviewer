import pytest

from code_reviewer.github_client import GitHubClient
from code_reviewer.models import InlineComment


def test_extract_valid_diff_lines_basic():
    client = GitHubClient(token="dummytoken")
    patch = """@@ -1,4 +1,4 @@\n-line a\n+line a updated\n line b\n+line c\n"""

    result = client._extract_valid_diff_lines(patch)

    # Lines 1, 2, and 3 in the new file are part of the diff
    assert result == {1, 2, 3}


@pytest.mark.asyncio
async def test_prepare_inline_comments_filters_invalid_lines(monkeypatch):
    async def fake_collect(self, owner, repo, pr_number):
        return {"src/file.py": {5, 6}}

    monkeypatch.setattr(GitHubClient, "_collect_valid_comment_lines", fake_collect)

    client = GitHubClient(token="dummytoken")
    inline_comments = [
        InlineComment(file="src/file.py", line=5, message="Keep this"),
        InlineComment(file="src/file.py", line=10, message="Drop this"),
    ]

    payload, dropped = await client.prepare_inline_comments([
        "owner",
        "repo",
    ], 42, inline_comments)

    assert payload == [
        {
            "path": "src/file.py",
            "line": 5,
            "side": "RIGHT",
            "body": "Keep this",
        }
    ]
    assert dropped == [InlineComment(file="src/file.py", line=10, message="Drop this")]


def test_format_dropped_inline_comments_output():
    client = GitHubClient(token="dummytoken")
    message = client.format_dropped_inline_comments([
        InlineComment(file="src/file.py", line=7, message="Needs update"),
    ])

    assert "src/file.py:7" in message
    assert "Needs update" in message
