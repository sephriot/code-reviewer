import pytest

from code_reviewer.database import ReviewDatabase


@pytest.mark.asyncio
async def test_review_started_comment_upsert_and_delete(tmp_path):
    db = ReviewDatabase(tmp_path / "reviews.db")

    try:
        await db.upsert_review_started_comment(
            "acme/repo",
            17,
            111,
            "deadbeef17",
        )

        first_comment = await db.get_review_started_comment("acme/repo", 17)
        assert first_comment is not None
        assert first_comment["comment_id"] == 111
        assert first_comment["head_sha"] == "deadbeef17"

        await db.upsert_review_started_comment(
            "acme/repo",
            17,
            222,
            "feedface17",
        )

        updated_comment = await db.get_review_started_comment("acme/repo", 17)
        assert updated_comment is not None
        assert updated_comment["comment_id"] == 222
        assert updated_comment["head_sha"] == "feedface17"

        await db.delete_review_started_comment("acme/repo", 17)
        assert await db.get_review_started_comment("acme/repo", 17) is None
    finally:
        db.close()
