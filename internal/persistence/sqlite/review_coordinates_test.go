package sqlite

import (
	"context"
	"errors"
	"testing"
)

func TestResolveReviewCoordinatesAndProfileVersion(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	profile, err := store.CreateReviewProfileVersion(ctx, testReviewProfileVersionInput())
	if err != nil {
		t.Fatal(err)
	}

	coordinate, err := store.ResolveReviewPullRequest(ctx, "connection-1", "OWNER", "REPO-1", 42)
	if err != nil {
		t.Fatal(err)
	}
	if coordinate.ConnectionID != "connection-1" || coordinate.RepositoryID != "repo-1" || coordinate.PullRequestID != "pr-1" ||
		coordinate.Owner != "owner" || coordinate.Repository != "repo-1" || coordinate.Number != 42 {
		t.Fatalf("coordinate = %+v", coordinate)
	}

	resolved, err := store.ResolveReviewProfileVersion(ctx, "DEFAULT", 1)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProfileID != profile.ProfileID || resolved.ProfileVersionID != profile.VersionID || resolved.ProfileKey != "default" || resolved.Version != 1 {
		t.Fatalf("profile = %+v", resolved)
	}
}

func TestResolveReviewCoordinatesRejectsMissingOrInactiveFacts(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	if _, err := store.ResolveReviewPullRequest(ctx, "connection-1", "owner", "repo-1", 43); !errors.Is(err, ErrReviewPullRequestNotFound) {
		t.Fatalf("missing pull request error = %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE connection_repositories SET access_state = 'removed'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveReviewPullRequest(ctx, "connection-1", "owner", "repo-1", 42); !errors.Is(err, ErrReviewPullRequestNotFound) {
		t.Fatalf("inactive repository error = %v", err)
	}
	if _, err := store.ResolveReviewProfileVersion(ctx, "default", 1); !errors.Is(err, ErrReviewProfileVersionNotFound) {
		t.Fatalf("missing profile error = %v", err)
	}
}
