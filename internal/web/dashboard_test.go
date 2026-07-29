package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
)

func TestDashboardQueueShowsFilteredPR(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "org/filtered", PRNumber: 42, Title: "draft pr", Author: "alice",
		CommitSHA: "abc", State: db.PRStateOpen, NeedsReview: false, FilteredReason: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateReviewRequest(prID); err != nil {
		t.Fatal(err)
	}

	s := New(&config.Config{}, d, nil, nil)
	rr := httptest.NewRecorder()
	s.dashboard(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, body)
	}
	if !strings.Contains(body, "org/filtered#42") {
		t.Fatalf("queue missing filtered PR label; body:\n%s", body)
	}
	if strings.Contains(body, ">#0</a>") {
		t.Fatalf("queue rendered #0; body:\n%s", body)
	}
	if !strings.Contains(body, `id="mute-notifications"`) {
		t.Fatalf("dashboard missing mute checkbox; body:\n%s", body)
	}
}
