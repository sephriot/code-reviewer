package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClientAllowsOnlySecureOrLoopbackEndpoints(t *testing.T) {
	if _, err := NewClient("http://example.com", "secret", nil); err == nil {
		t.Fatal("NewClient accepted non-loopback HTTP")
	}
	if _, err := NewClient("https://api.github.com", "", nil); err == nil {
		t.Fatal("NewClient accepted empty token")
	}
	if _, err := NewClient("http://127.0.0.1:8080", "secret", nil); err != nil {
		t.Fatalf("NewClient rejected loopback test server: %v", err)
	}
}

func TestAuthenticatedUserUsesReadHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/user" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("X-GitHub-Api-Version") != apiVersion || request.Header.Get("User-Agent") == "" {
			t.Errorf("required headers missing: %v", request.Header)
		}
		_, _ = response.Write([]byte(`{"id":7,"node_id":"U_7","login":"reviewer"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	user, err := client.AuthenticatedUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if user.User.ID != 7 || user.User.Login != "reviewer" {
		t.Fatalf("user = %+v", user)
	}
}

func TestSearchPullRequestsPreservesCompletenessAndPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/search/issues" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("q") != "is:pr state:open review-requested:reviewer" || query.Get("page") != "1" || query.Get("per_page") != "100" {
			t.Errorf("query = %v", query)
		}
		response.Header().Set("Link", `<`+serverURL(request)+`/search/issues?page=2>; rel="next", <`+serverURL(request)+`/search/issues?page=3>; rel="last"`)
		response.Header().Set("X-RateLimit-Limit", "30")
		response.Header().Set("X-RateLimit-Remaining", "12")
		response.Header().Set("X-RateLimit-Used", "18")
		response.Header().Set("X-RateLimit-Resource", "search")
		_, _ = response.Write([]byte(`{
          "total_count": 140,
          "incomplete_results": true,
          "items": [
            {"id":999,"number":42,"repository_url":"https://api.github.com/repos/acme/widgets","pull_request":{}},
            {"id":1000,"number":8,"repository_url":"https://api.github.com/repos/acme/issue-only"}
          ]
        }`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "test-token", server.Client())
	page, err := client.SearchPullRequests(context.Background(), "is:pr state:open review-requested:reviewer", 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalCount != 140 || !page.IncompleteResults || page.NextPage != 2 || len(page.Candidates) != 1 {
		t.Fatalf("page = %+v", page)
	}
	if page.RateLimit.Limit != 30 || page.RateLimit.Remaining != 12 || page.RateLimit.Used != 18 || page.RateLimit.Resource != "search" {
		t.Fatalf("rate limit = %+v", page.RateLimit)
	}
	if page.Candidates[0] != (SearchCandidate{Owner: "acme", Repository: "widgets", Number: 42}) {
		t.Fatalf("candidate = %+v", page.Candidates[0])
	}
}

func TestGetPullRequestUsesBaseRepositoryAndCanonicalIdentity(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls++
		if request.URL.Path != "/repos/acme/widgets/pulls/42" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if calls == 2 {
			if request.Header.Get("If-None-Match") != `"etag-1"` {
				t.Errorf("If-None-Match = %q", request.Header.Get("If-None-Match"))
			}
			response.WriteHeader(http.StatusNotModified)
			return
		}
		response.Header().Set("ETag", `"etag-1"`)
		_, _ = response.Write([]byte(`{
          "id":501,"node_id":"PR_501","number":42,"html_url":"https://github.com/acme/widgets/pull/42",
          "title":"Canonical PR","body":"Details","user":{"id":9,"node_id":"U_9","login":"author"},
          "state":"open","merged":false,"draft":false,"updated_at":"2026-07-21T08:00:00Z",
          "head":{"sha":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","repo":{"id":999,"full_name":"fork/widgets"}},
          "base":{"sha":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","ref":"main","repo":{"id":77,"node_id":"R_77","full_name":"acme/widgets"}},
          "labels":[{"name":"zeta"},{"name":"bug"}],
          "requested_reviewers":[{"id":12,"node_id":"U_12","login":"later"},{"id":7,"node_id":"U_7","login":"reviewer"}]
        }`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "test-token", server.Client())
	result, err := client.GetPullRequest(context.Background(), "acme", "widgets", 42, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.PullRequest.ID != 501 || result.PullRequest.TargetRepository.ID != 77 || result.PullRequest.TargetRepository.FullName != "acme/widgets" {
		t.Fatalf("pull request identity = %+v", result.PullRequest)
	}
	if result.PullRequest.HeadSHA != strings.Repeat("a", 40) || result.PullRequest.BaseSHA != strings.Repeat("b", 40) {
		t.Fatalf("SHAs = %q %q", result.PullRequest.HeadSHA, result.PullRequest.BaseSHA)
	}
	if strings.Join(result.PullRequest.Labels, ",") != "bug,zeta" || result.PullRequest.RequestedReviewers[0].ID != 7 {
		t.Fatalf("normalized policy facts = %+v", result.PullRequest)
	}
	notModified, err := client.GetPullRequest(context.Background(), "acme", "widgets", 42, `"etag-1"`)
	if err != nil {
		t.Fatal(err)
	}
	if !notModified.NotModified || notModified.PullRequest != nil || notModified.ETag != `"etag-1"` {
		t.Fatalf("not modified result = %+v", notModified)
	}
}

func TestClientReturnsTypedRateLimitWithoutToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Retry-After", "5")
		response.Header().Set("X-RateLimit-Reset", "2000000000")
		response.WriteHeader(http.StatusForbidden)
		_, _ = response.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "super-secret", server.Client())
	_, err := client.AuthenticatedUser(context.Background())
	var httpError *HTTPError
	if !errors.As(err, &httpError) {
		t.Fatalf("error = %v", err)
	}
	if httpError.RateLimit.RetryAfter != 5*time.Second || httpError.RateLimit.Reset.Unix() != 2000000000 {
		t.Fatalf("rate metadata = %+v", httpError)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatal("error disclosed token")
	}
}

func TestClientRedactsProviderMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusBadRequest)
		_, _ = response.Write([]byte("{\"message\":\"bad super-secret\\nheader\"}"))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "super-secret", server.Client())
	_, err := client.AuthenticatedUser(context.Background())
	if err == nil || strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("unsanitized error = %q", err)
	}
}

func TestSearchPullRequestsRejectsRegressiveNextPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Link", `<`+serverURL(request)+`/search/issues?page=1>; rel="next"`)
		_, _ = response.Write([]byte(`{"total_count":1,"items":[]}`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "test-token", server.Client())
	if _, err := client.SearchPullRequests(context.Background(), "is:pr", 1); err == nil {
		t.Fatal("SearchPullRequests accepted regressive next page")
	}
}

func TestSearchPullRequestsRejectsSkippedNextPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Link", `<`+serverURL(request)+`/search/issues?page=3>; rel="next"`)
		_, _ = response.Write([]byte(`{"total_count":1,"items":[]}`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "test-token", server.Client())
	if _, err := client.SearchPullRequests(context.Background(), "is:pr", 1); err == nil {
		t.Fatal("SearchPullRequests accepted skipped next page")
	}
}

func TestGetPullRequestRejectsUnconditionalNotModified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "test-token", server.Client())
	if _, err := client.GetPullRequest(context.Background(), "acme", "widgets", 42, ""); err == nil {
		t.Fatal("GetPullRequest accepted unconditional 304")
	}
}

func TestGetPullRequestRejectsMismatchedRequestedIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(`{
          "id":501,"node_id":"PR_501","number":99,"user":{"id":9,"login":"author"},
          "state":"open","updated_at":"2026-07-21T08:00:00Z",
          "head":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
          "base":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","ref":"main","repo":{"id":77,"node_id":"R_77","full_name":"other/repo"}}
        }`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "test-token", server.Client())
	if _, err := client.GetPullRequest(context.Background(), "acme", "widgets", 42, ""); err == nil {
		t.Fatal("GetPullRequest accepted mismatched response identity")
	}
}

func TestClientDoesNotForwardCredentialsThroughRedirects(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		redirected = true
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL, http.StatusFound)
	}))
	defer source.Close()
	client, _ := NewClient(source.URL, "super-secret", source.Client())
	_, err := client.AuthenticatedUser(context.Background())
	var httpError *HTTPError
	if !errors.As(err, &httpError) || httpError.StatusCode != http.StatusFound {
		t.Fatalf("redirect error = %v", err)
	}
	if redirected {
		t.Fatal("client followed redirect")
	}
}

func TestClientFollowsSameOriginRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/user" {
			http.Redirect(response, request, "/renamed-user", http.StatusMovedPermanently)
			return
		}
		if request.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("redirect lost authorization: %q", request.Header.Get("Authorization"))
		}
		_, _ = response.Write([]byte(`{"id":7,"login":"reviewer"}`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "test-token", server.Client())
	if _, err := client.AuthenticatedUser(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGetPullRequestRejectsPlaceholderSHA(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(`{
          "id":501,"number":42,"user":{"id":9,"login":"author"},"updated_at":"2026-07-21T08:00:00Z",
          "head":{"sha":"deadbeef"},"base":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","repo":{"id":77,"full_name":"acme/widgets"}}
        }`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "test-token", server.Client())
	if _, err := client.GetPullRequest(context.Background(), "acme", "widgets", 42, ""); err == nil {
		t.Fatal("GetPullRequest accepted placeholder SHA")
	}
}

func serverURL(request *http.Request) string { return "http://" + request.Host }
