package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	gogh "github.com/google/go-github/v68/github"
)

func TestListAssignedPRsPaginates(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("Content-Type", "application/json")
		page := r.URL.Query().Get("page")
		mk := func(n int) *gogh.Issue {
			return &gogh.Issue{
				Number:         gogh.Int(n),
				Title:          gogh.String(fmt.Sprintf("PR %d", n)),
				User:           &gogh.User{Login: gogh.String("dev")},
				RepositoryURL:  gogh.String("https://api.github.com/repos/spacelift-io/backend"),
				PullRequestLinks: &gogh.PullRequestLinks{URL: gogh.String("https://api.github.com/repos/spacelift-io/backend/pulls/1")},
			}
		}
		if page == "" || page == "1" {
			w.Header().Set("Link", `<`+r.URL.Path+`?q=x&page=2>; rel="next"`)
			_ = json.NewEncoder(w).Encode(&gogh.IssuesSearchResult{
				Total:             gogh.Int(2),
				IncompleteResults: gogh.Bool(false),
				Issues:            []*gogh.Issue{mk(1)},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(&gogh.IssuesSearchResult{
			Total:             gogh.Int(2),
			IncompleteResults: gogh.Bool(false),
			Issues:            []*gogh.Issue{mk(2)},
		})
	}))
	defer srv.Close()

	c := &Client{Client: gogh.NewClient(srv.Client()), username: "alice"}
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = base

	prs, err := c.ListAssignedPRs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pages < 2 {
		t.Fatalf("expected pagination, pages=%d", pages)
	}
	if len(prs) != 2 {
		t.Fatalf("want 2 PRs across pages, got %d", len(prs))
	}
}
