package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	gogh "github.com/google/go-github/v68/github"
)

func TestHasUserReviewedMatchesCommitSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]*gogh.PullRequestReview{
			{
				User:     &gogh.User{Login: gogh.String("alice")},
				CommitID: gogh.String("old-sha"),
				State:    gogh.String("APPROVED"),
			},
			{
				User:     &gogh.User{Login: gogh.String("bob")},
				CommitID: gogh.String("new-sha"),
				State:    gogh.String("APPROVED"),
			},
		})
	}))
	defer srv.Close()

	c := &Client{Client: gogh.NewClient(srv.Client()), username: "alice"}
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = base

	ok, err := c.HasUserReviewed(context.Background(), "o", "r", 1, "new-sha")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("alice approved old-sha only; new-sha should be false")
	}

	ok, err = c.HasUserReviewed(context.Background(), "o", "r", 1, "old-sha")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("alice approved old-sha; expected true")
	}
}

func TestHasUserReviewedPaginates(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("Content-Type", "application/json")
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			w.Header().Set("Link", `<`+r.URL.Path+`?page=2>; rel="next"`)
			_ = json.NewEncoder(w).Encode([]*gogh.PullRequestReview{
				{User: &gogh.User{Login: gogh.String("other")}, CommitID: gogh.String("sha")},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]*gogh.PullRequestReview{
			{User: &gogh.User{Login: gogh.String("alice")}, CommitID: gogh.String("sha")},
		})
	}))
	defer srv.Close()

	c := &Client{Client: gogh.NewClient(srv.Client()), username: "alice"}
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = base

	ok, err := c.HasUserReviewed(context.Background(), "o", "r", 1, "sha")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected review on page 2")
	}
	if pages < 2 {
		t.Fatalf("expected pagination, pages=%d", pages)
	}
}
