package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gogh "github.com/google/go-github/v68/github"
)

func TestListReviewAssignmentsUnionsDirectAndTeamPages(t *testing.T) {
	searchPages := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/teams":
			_ = json.NewEncoder(w).Encode([]*gogh.Team{
				{
					Slug: gogh.String("platform"),
					Organization: &gogh.Organization{
						Login: gogh.String("acme"),
					},
				},
			})
		case "/search/issues":
			query := r.URL.Query().Get("q")
			page := r.URL.Query().Get("page")
			searchPages[query]++
			switch {
			case strings.Contains(query, "review-requested:alice") && page == "":
				w.Header().Set("Link", `<`+r.URL.Path+`?q=`+url.QueryEscape(query)+`&page=2>; rel="next"`)
				writeIssueSearch(t, w, 1)
			case strings.Contains(query, "review-requested:alice") && page == "2":
				writeIssueSearch(t, w, 2)
			case strings.Contains(query, "team-review-requested:acme/platform"):
				writeIssueSearch(t, w, 2, 3)
			default:
				t.Fatalf("unexpected search query %q page %q", query, page)
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := testClient(t, srv, "alice")
	snapshot, err := client.ListReviewAssignments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Complete {
		t.Fatal("successful discovery must be complete")
	}
	if len(snapshot.PRs) != 3 {
		t.Fatalf("got %d deduplicated PRs, want 3", len(snapshot.PRs))
	}
	if searchPages["is:open is:pr review-requested:alice"] != 2 {
		t.Fatalf("direct search pages = %d, want 2", searchPages["is:open is:pr review-requested:alice"])
	}
	if searchPages["is:open is:pr team-review-requested:acme/platform"] != 1 {
		t.Fatalf("team search pages = %d, want 1", searchPages["is:open is:pr team-review-requested:acme/platform"])
	}
}

func TestListReviewAssignmentsReturnsPartialSnapshotOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/teams":
			_ = json.NewEncoder(w).Encode([]*gogh.Team{
				{
					Slug: gogh.String("platform"),
					Organization: &gogh.Organization{
						Login: gogh.String("acme"),
					},
				},
			})
		case "/search/issues":
			if strings.Contains(r.URL.Query().Get("q"), "team-review-requested:") {
				http.Error(w, `{"message":"failed"}`, http.StatusInternalServerError)
				return
			}
			writeIssueSearch(t, w, 1)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := testClient(t, srv, "alice")
	snapshot, err := client.ListReviewAssignments(context.Background())
	if err == nil {
		t.Fatal("team search failure must be visible")
	}
	if snapshot.Complete {
		t.Fatal("failed team search must make snapshot incomplete")
	}
	if len(snapshot.PRs) != 1 || snapshot.PRs[0].Number != 1 {
		t.Fatalf("partial snapshot = %#v, want direct assignment", snapshot.PRs)
	}
}

func writeIssueSearch(t *testing.T, w http.ResponseWriter, numbers ...int) {
	t.Helper()
	issues := make([]*gogh.Issue, 0, len(numbers))
	for _, number := range numbers {
		issues = append(issues, &gogh.Issue{
			Number:           gogh.Int(number),
			Title:            gogh.String("PR"),
			User:             &gogh.User{Login: gogh.String("dev")},
			RepositoryURL:    gogh.String("https://api.github.com/repos/acme/repo"),
			PullRequestLinks: &gogh.PullRequestLinks{URL: gogh.String("https://api.github.com/repos/acme/repo/pulls/1")},
		})
	}
	_ = json.NewEncoder(w).Encode(&gogh.IssuesSearchResult{
		Total:             gogh.Int(len(issues)),
		IncompleteResults: gogh.Bool(false),
		Issues:            issues,
	})
}

func testClient(t *testing.T, server *httptest.Server, username string) *Client {
	t.Helper()
	client := &Client{Client: gogh.NewClient(server.Client()), username: username}
	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client.BaseURL = base
	return client
}
