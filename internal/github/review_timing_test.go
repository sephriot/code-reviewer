package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFirstReviewTimeUsesFirstSubmittedRelevantReviewAcrossPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "100" {
			t.Error("reviews not paginated")
		}
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"user":{"login":"Me","type":"User"},"state":"DISMISSED","submitted_at":"2026-10-01T02:00:00Z"},{"user":{"login":"human","type":"User"},"state":"CHANGES_REQUESTED","submitted_at":"2026-10-01T01:00:00Z"}]`)
			return
		}
		w.Header().Set("Link", `<http://`+r.Host+r.URL.Path+`?page=2&per_page=100>; rel="next"`)
		fmt.Fprint(w, `[
   {"user":{"login":"author"},"state":"COMMENTED","submitted_at":"2026-10-01T00:01:00Z"},
   {"user":{"login":"bot","type":"Bot"},"state":"APPROVED","submitted_at":"2026-10-01T00:02:00Z"},
   {"user":{"login":"robot[bot]"},"state":"APPROVED","submitted_at":"2026-10-01T00:03:00Z"},
   {"user":{"login":"me"},"state":"PENDING","submitted_at":"2026-10-01T00:04:00Z"},
   {"user":{"login":"human"},"state":"COMMENTED"},
   {"user":{"login":"me"},"state":"APPROVED","submitted_at":"2026-10-01T03:00:00Z"}
  ]`)
	}))
	defer srv.Close()
	client := testClient(t, srv, "me")
	opened, _ := time.Parse(time.RFC3339, "2026-10-01T00:00:00Z")
	pr := TimingCandidate{Owner: "o", Repo: "r", Number: 1, Author: "Author", CreatedAt: opened}
	for _, tc := range []struct {
		direction string
		hours     float64
	}{{"given", 2}, {"received", 1}} {
		first, err := client.FirstReviewTime(context.Background(), pr, tc.direction)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil || first.Sub(opened).Hours() != tc.hours {
			t.Fatalf("%s: %v", tc.direction, first)
		}
	}
}

func TestWalkTimingPRsSplitsSearchCapAndPaginates(t *testing.T) {
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	before := start.AddDate(0, 0, 2)
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("q")
		queries = append(queries, query)
		if !strings.Contains(query, "reviewed-by:me created:") {
			t.Error(query)
		}
		if !strings.Contains(query, "-author:me") || !strings.Contains(query, "updated:>=2026-10-01T00:00:00Z") {
			t.Error(query)
		}
		if strings.Contains(query, "2026-10-01T00:00:00Z..2026-10-02T23:59:59Z") {
			fmt.Fprint(w, `{"total_count":1001,"items":[]}`)
			return
		}
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<http://`+r.Host+`/search/issues?page=2>; rel="next"`)
		}
		number := len(queries)
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "items": []any{map[string]any{
			"number": number, "user": map[string]string{"login": "author"}, "repository_url": "https://api.github.com/repos/o/r",
			"pull_request": map[string]string{"url": "https://api.github.com/repos/o/r/pulls/1"}, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-02T00:00:00Z",
		}}})
	}))
	defer srv.Close()
	count := 0
	err := testClient(t, srv, "me").WalkTimingPRs(context.Background(), "given", start, before, &start, func(pr TimingCandidate) error {
		count++
		if pr.Owner != "o" || pr.Repo != "r" || pr.CreatedAt != start {
			t.Fatalf("candidate: %+v", pr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 || len(queries) != 5 {
		t.Fatalf("visited %d PRs over %d queries", count, len(queries))
	}
}

func TestWalkTimingPRsIncompleteSmallestRangeFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"total_count":1,"incomplete_results":true,"items":[]}`)
	}))
	defer srv.Close()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	err := testClient(t, srv, "me").WalkTimingPRs(context.Background(), "received", start, start.Add(time.Second), nil, func(TimingCandidate) error { t.Fatal("visited incomplete results"); return nil })
	if err == nil {
		t.Fatal("accepted incomplete search")
	}
}
