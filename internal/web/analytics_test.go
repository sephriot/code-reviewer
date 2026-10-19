package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
)

func TestAnalyticsIncludesGitHubTimingWithoutLocalReviews(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "timing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	state, err := d.TimingSyncState("me", now)
	if err != nil {
		t.Fatal(err)
	}
	state.LastError = "GitHub rate limited"
	if err := d.SaveTimingSyncState("me", state); err != nil {
		t.Fatal(err)
	}
	sample := db.ReviewTiming{Repo: "o/r", PRNumber: 1, Direction: "given", OpenedAt: now.Add(-4 * time.Hour), ReviewedAt: &now, UpdatedAt: now}
	if err := d.UpsertReviewTiming("me", sample); err != nil {
		t.Fatal(err)
	}
	s := New(&config.Config{GithubUsername: "me"}, d, nil, nil)
	rr := httptest.NewRecorder()
	s.apiAnalytics(rr, httptest.NewRequest(http.MethodGet, "/api/analytics?period=year", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("%d: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		Total  int                      `json:"total"`
		Timing db.ReviewTimingAnalytics `json:"review_timing"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 || result.Timing.Given.Count != 1 || *result.Timing.Given.MedianHours != 4 || result.Timing.Sync.LastError != state.LastError {
		t.Fatalf("response: %+v", result)
	}
	if len(result.Timing.Given.Daily) < 365 {
		t.Fatal("year timing is not daily")
	}
	rr = httptest.NewRecorder()
	s.analyticsPage(rr, httptest.NewRequest(http.MethodGet, "/analytics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("page: %d %s", rr.Code, rr.Body.String())
	}
	for _, content := range []string{`id="timing-histogram"`, `id="timing-daily"`, `id="timing-sync"`, `id="timing-buckets"`, `id="timing-days"`} {
		if !strings.Contains(rr.Body.String(), content) {
			t.Errorf("page missing %s", content)
		}
	}
}
