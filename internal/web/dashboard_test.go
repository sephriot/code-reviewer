package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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

func TestFilteredShowsReviewedExternally(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "org/filt", PRNumber: 7, Title: "filtered reviewed", Author: "alice",
		CommitSHA: "sha7", State: db.PRStateOpen, NeedsReview: false, FilteredReason: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateExternalReview(prID, "sha7"); err != nil {
		t.Fatal(err)
	}

	s := New(&config.Config{}, d, nil, nil)
	rr := httptest.NewRecorder()
	s.filteredPRs(rr, httptest.NewRequest(http.MethodGet, "/filtered", nil))

	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, body)
	}
	if !strings.Contains(body, "reviewed_externally") {
		t.Fatalf("filtered page missing reviewed_externally badge; body:\n%s", body)
	}
}

func TestHistoryShowsReviewedExternallyForOpenPR(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "org/hist", PRNumber: 8, Title: "externally reviewed open", Author: "bob",
		CommitSHA: "sha8", State: db.PRStateOpen, NeedsReview: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateExternalReview(prID, "sha8"); err != nil {
		t.Fatal(err)
	}

	s := New(&config.Config{}, d, nil, nil)
	rr := httptest.NewRecorder()
	s.historyPage(rr, httptest.NewRequest(http.MethodGet, "/history", nil))

	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, body)
	}
	if !strings.Contains(body, "reviewed_externally") {
		t.Fatalf("history missing reviewed_externally badge; body:\n%s", body)
	}
}

func TestPRDetailShowsLatestOutcomeWhenPublished(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "org/detail", PRNumber: 9, Title: "externally reviewed", Author: "carol",
		CommitSHA: "sha9", State: db.PRStateOpen, NeedsReview: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateExternalReview(prID, "sha9"); err != nil {
		t.Fatal(err)
	}

	s := New(&config.Config{}, d, nil, nil)
	rr := httptest.NewRecorder()
	s.prDetail(rr, httptest.NewRequest(http.MethodGet, "/pr/"+strconv.FormatInt(prID, 10), nil))

	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, body)
	}
	if !strings.Contains(body, `class="outcome-badge reviewed_externally"`) {
		t.Fatalf("PR detail missing reviewed_externally header badge; body:\n%s", body)
	}
}

func TestFilteredAndHistoryIncludeGitHubLink(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	_, err = d.UpsertPR(db.PullRequest{
		Repo: "org/filt", PRNumber: 11, Title: "filtered", Author: "alice",
		CommitSHA: "s1", State: db.PRStateOpen, NeedsReview: false, FilteredReason: "author",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertPR(db.PullRequest{
		Repo: "org/hist", PRNumber: 12, Title: "history", Author: "bob",
		CommitSHA: "s2", State: db.PRStateMerged, NeedsReview: false,
	}); err != nil {
		t.Fatal(err)
	}
	s := New(&config.Config{}, d, nil, nil)

	frr := httptest.NewRecorder()
	s.filteredPRs(frr, httptest.NewRequest(http.MethodGet, "/filtered", nil))
	fbody := frr.Body.String()
	if frr.Code != http.StatusOK {
		t.Fatalf("filtered status %d", frr.Code)
	}
	if !strings.Contains(fbody, `class="gh-link"`) || !strings.Contains(fbody, "https://github.com/org/filt/pull/11") {
		t.Fatalf("filtered missing GitHub link; body:\n%s", fbody)
	}

	hrr := httptest.NewRecorder()
	s.historyPage(hrr, httptest.NewRequest(http.MethodGet, "/history", nil))
	hbody := hrr.Body.String()
	if hrr.Code != http.StatusOK {
		t.Fatalf("history status %d", hrr.Code)
	}
	if !strings.Contains(hbody, `class="gh-link"`) || !strings.Contains(hbody, "https://github.com/org/hist/pull/12") {
		t.Fatalf("history missing GitHub link; body:\n%s", hbody)
	}
}
