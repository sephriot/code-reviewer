package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gogh "github.com/google/go-github/v68/github"
)

func TestGetEffectiveReviewUsesLatestAuthenticatedUserReview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<`+r.URL.Path+`?page=2>; rel="next"`)
			_ = json.NewEncoder(w).Encode([]*gogh.PullRequestReview{
				reviewFixture(1, "alice", "APPROVED", "2026-07-01T10:00:00Z"),
				reviewFixture(2, "bob", "CHANGES_REQUESTED", "2026-07-03T10:00:00Z"),
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]*gogh.PullRequestReview{
			reviewFixture(3, "alice", "CHANGES_REQUESTED", "2026-07-02T10:00:00Z"),
		})
	}))
	defer srv.Close()

	client := testClient(t, srv, "alice")
	review, err := client.GetEffectiveReview(context.Background(), "acme", "repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if review == nil || review.ID != 3 || review.State != ReviewStateChangesRequested {
		t.Fatalf("effective review = %#v, want id=3 changes_requested", review)
	}
}

func TestGetEffectiveReviewDropsDismissedOrPendingLatestReview(t *testing.T) {
	for _, state := range []string{"DISMISSED", "PENDING"} {
		t.Run(state, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]*gogh.PullRequestReview{
					reviewFixture(1, "alice", "APPROVED", "2026-07-01T10:00:00Z"),
					reviewFixture(2, "alice", state, "2026-07-02T10:00:00Z"),
				})
			}))
			defer srv.Close()

			client := testClient(t, srv, "alice")
			review, err := client.GetEffectiveReview(context.Background(), "acme", "repo", 1)
			if err != nil {
				t.Fatal(err)
			}
			if review != nil {
				t.Fatalf("effective review = %#v, want nil", review)
			}
		})
	}
}

func TestGetEffectiveReviewTreatsHigherIDPendingReviewWithoutSubmittedTimeAsLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]*gogh.PullRequestReview{
			reviewFixture(10, "alice", "APPROVED", "2026-07-01T10:00:00Z"),
			{
				ID:    gogh.Int64(11),
				User:  &gogh.User{Login: gogh.String("alice")},
				State: gogh.String("PENDING"),
			},
		})
	}))
	defer srv.Close()

	client := testClient(t, srv, "alice")
	review, err := client.GetEffectiveReview(context.Background(), "acme", "repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if review != nil {
		t.Fatalf("effective review = %#v, want nil after pending review", review)
	}
}

func TestSubmitReviewReturnsGitHubReviewID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(&gogh.PullRequestReview{ID: gogh.Int64(987)})
	}))
	defer srv.Close()

	client := testClient(t, srv, "alice")
	id, err := client.SubmitReview(context.Background(), "acme", "repo", 1, ReviewSubmission{
		Outcome: "approve_without_comments",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != 987 {
		t.Fatalf("review id = %d, want 987", id)
	}
}

func TestSubmitReviewPublishesChangesRequestedEvent(t *testing.T) {
	var event string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body gogh.PullRequestReviewRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		event = body.GetEvent()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(&gogh.PullRequestReview{ID: gogh.Int64(988)})
	}))
	defer srv.Close()

	client := testClient(t, srv, "alice")
	if _, err := client.SubmitReview(context.Background(), "acme", "repo", 1, ReviewSubmission{
		Outcome: "changes_requested",
	}); err != nil {
		t.Fatal(err)
	}
	if event != "REQUEST_CHANGES" {
		t.Fatalf("event = %q, want REQUEST_CHANGES", event)
	}
}

func reviewFixture(id int64, login, state, submitted string) *gogh.PullRequestReview {
	at, _ := time.Parse(time.RFC3339, submitted)
	return &gogh.PullRequestReview{
		ID:          gogh.Int64(id),
		User:        &gogh.User{Login: gogh.String(login)},
		State:       gogh.String(state),
		SubmittedAt: &gogh.Timestamp{Time: at},
	}
}
