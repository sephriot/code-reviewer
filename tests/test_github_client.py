import pytest

from code_reviewer.github_client import GitHubClient, _matches_repository_filter
from code_reviewer.models import InlineComment


class TestMatchesRepositoryFilter:
    def test_exact_match(self):
        assert _matches_repository_filter("spacelift-io/worker-pool", ["spacelift-io/worker-pool"])

    def test_exact_no_match(self):
        assert not _matches_repository_filter("spacelift-io/worker-pool", ["spacelift-io/other"])

    def test_wildcard_org(self):
        assert _matches_repository_filter("spacelift-io/worker-pool", ["spacelift-io/*"])
        assert _matches_repository_filter("spacelift-io/backend", ["spacelift-io/*"])
        assert not _matches_repository_filter("other-org/backend", ["spacelift-io/*"])

    def test_wildcard_prefix(self):
        assert _matches_repository_filter("spacelift-io/worker-pool", ["spacelift-io/worker-*"])
        assert not _matches_repository_filter("spacelift-io/backend", ["spacelift-io/worker-*"])

    def test_question_mark(self):
        assert _matches_repository_filter("spacelift-io/app-v1", ["spacelift-io/app-v?"])
        assert not _matches_repository_filter("spacelift-io/app-v12", ["spacelift-io/app-v?"])

    def test_multiple_patterns(self):
        patterns = ["spacelift-io/*", "other-org/specific-repo"]
        assert _matches_repository_filter("spacelift-io/anything", patterns)
        assert _matches_repository_filter("other-org/specific-repo", patterns)
        assert not _matches_repository_filter("other-org/other-repo", patterns)

    def test_empty_patterns(self):
        assert not _matches_repository_filter("spacelift-io/repo", [])


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


class _FakeResponse:
    def __init__(self, status, payload=None, text=""):
        self.status = status
        self._payload = payload or {}
        self._text = text

    async def __aenter__(self):
        return self

    async def __aexit__(self, exc_type, exc, tb):
        return False

    async def json(self):
        return self._payload

    async def text(self):
        return self._text


class _FakeSession:
    def __init__(self):
        self.closed = False
        self.post_calls = []
        self.delete_calls = []

    def post(self, url, json):
        self.post_calls.append((url, json))
        return _FakeResponse(201, payload={"id": 12345})

    def delete(self, url):
        self.delete_calls.append(url)
        return _FakeResponse(204)


@pytest.mark.asyncio
async def test_add_issue_comment_uses_issue_comment_endpoint():
    client = GitHubClient(token="dummytoken")
    client.session = _FakeSession()

    comment_id = await client.add_issue_comment(["owner", "repo"], 42, "👀")

    assert comment_id == 12345
    assert client.session.post_calls == [
        (
            "https://api.github.com/repos/owner/repo/issues/42/comments",
            {"body": "👀"},
        )
    ]


@pytest.mark.asyncio
async def test_delete_issue_comment_uses_issue_comment_delete_endpoint():
    client = GitHubClient(token="dummytoken")
    client.session = _FakeSession()

    deleted = await client.delete_issue_comment(["owner", "repo"], 12345)

    assert deleted is True
    assert client.session.delete_calls == [
        "https://api.github.com/repos/owner/repo/issues/comments/12345"
    ]
