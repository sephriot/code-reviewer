package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
	gh "github.com/sephriot/code-reviewer/internal/github"
)

type fakeWebGitHub struct {
	submittedID int64
}

func (f *fakeWebGitHub) SubmitReview(context.Context, string, string, int, gh.ReviewSubmission) (int64, error) {
	return f.submittedID, nil
}

func (f *fakeWebGitHub) CreateReviewComment(context.Context, string, string, int, gh.ReviewComment) error {
	return nil
}

func (f *fakeWebGitHub) GetFileContent(context.Context, string, string, string, string, int, int) (string, int, error) {
	return "", 0, nil
}

func TestDashboardQueueShowsFilteredPR(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "org/filtered", PRNumber: 42, Title: "draft pr", Author: "alice",
		CommitSHA: "abc", State: db.PRStateOpen, IsAssigned: true, FilteredReason: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateReviewRequest(prID, "abc"); err != nil {
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

	reviewID := int64(70)
	_, err = d.UpsertPR(db.PullRequest{
		Repo: "org/filt", PRNumber: 7, Title: "filtered reviewed", Author: "alice",
		CommitSHA: "sha7", State: db.PRStateOpen, IsAssigned: true, FilteredReason: "author",
		EffectiveReviewID: &reviewID, EffectiveReviewState: db.EffectiveReviewStateCommented,
	})
	if err != nil {
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

	reviewID := int64(80)
	_, err = d.UpsertPR(db.PullRequest{
		Repo: "org/hist", PRNumber: 8, Title: "externally reviewed open", Author: "bob",
		CommitSHA: "sha8", State: db.PRStateOpen,
		EffectiveReviewID: &reviewID, EffectiveReviewState: db.EffectiveReviewStateCommented,
	})
	if err != nil {
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

	reviewID := int64(90)
	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "org/detail", PRNumber: 9, Title: "externally reviewed", Author: "carol",
		CommitSHA: "sha9", State: db.PRStateOpen, IsAssigned: true,
		EffectiveReviewID: &reviewID, EffectiveReviewState: db.EffectiveReviewStateCommented,
	})
	if err != nil {
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
		CommitSHA: "s1", State: db.PRStateOpen, IsAssigned: true, FilteredReason: "author",
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

func TestDashboardUsesExactGitHubReviewIDForProvenance(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	githubReviewID := int64(123)
	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "org/repo", PRNumber: 13, Title: "correlated", Author: "alice",
		CommitSHA: "sha13", State: db.PRStateOpen, IsAssigned: true,
		EffectiveReviewID: &githubReviewID, EffectiveReviewState: db.EffectiveReviewStateApproved,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := d.CreateReviewRequest(prID, "sha13")
	if err != nil {
		t.Fatal(err)
	}
	localReviewID, err := d.CreateReview(db.Review{
		PullRequestID: prID, ReviewRequestID: requestID,
		Outcome: db.ReviewOutcomeApproveWithComments, CommitSHA: "sha13",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PublishReview(localReviewID, githubReviewID); err != nil {
		t.Fatal(err)
	}

	server := New(&config.Config{}, d, nil, nil)
	recorder := httptest.NewRecorder()
	server.dashboard(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, db.ReviewOutcomeApproveWithComments) {
		t.Fatalf("dashboard missing correlated app outcome; body:\n%s", body)
	}
	if strings.Contains(body, "approved_externally") {
		t.Fatalf("correlated review labeled external; body:\n%s", body)
	}
}

func TestManualRetryRequiresDashboardEligibility(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "org/repo", PRNumber: 14, Title: "filtered", Author: "alice",
		CommitSHA: "sha14", State: db.PRStateOpen, IsAssigned: true, FilteredReason: "author",
	})
	if err != nil {
		t.Fatal(err)
	}

	server := New(&config.Config{}, d, nil, nil)
	recorder := httptest.NewRecorder()
	server.requestReview(recorder, httptest.NewRequest(http.MethodPost, "/", nil), prID)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
	}
	requests, err := d.ListReviewRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatalf("filtered PR queued manually: %#v", requests)
	}
}

func TestPublishReviewStoresGitHubReviewID(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "org/repo", PRNumber: 15, Title: "publish", Author: "alice",
		CommitSHA: "sha15", State: db.PRStateOpen, IsAssigned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := d.CreateReviewRequest(prID, "sha15")
	if err != nil {
		t.Fatal(err)
	}
	reviewID, err := d.CreateReview(db.Review{
		PullRequestID: prID, ReviewRequestID: requestID,
		Outcome: db.ReviewOutcomeChangesRequested, CommitSHA: "sha15",
	})
	if err != nil {
		t.Fatal(err)
	}

	server := New(&config.Config{}, d, &fakeWebGitHub{submittedID: 555}, nil)
	recorder := httptest.NewRecorder()
	server.publishReview(recorder, &db.Review{
		ID: reviewID, PullRequestID: prID, Outcome: db.ReviewOutcomeChangesRequested,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	review, err := d.GetReview(reviewID)
	if err != nil {
		t.Fatal(err)
	}
	if review.GitHubReviewID == nil || *review.GitHubReviewID != 555 {
		t.Fatalf("published review = %#v, want GitHub ID 555", review)
	}
}

func TestEffectiveReviewLabelsMapUncorrelatedGitHubStates(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	server := New(&config.Config{}, d, nil, nil)

	tests := []struct {
		state string
		want  string
	}{
		{db.EffectiveReviewStateApproved, "approved_externally"},
		{db.EffectiveReviewStateChangesRequested, "changes_requested_externally"},
	}
	for i, test := range tests {
		reviewID := int64(600 + i)
		prID, err := d.UpsertPR(db.PullRequest{
			Repo: "org/labels", PRNumber: 20 + i, Title: "label", Author: "alice",
			CommitSHA: "sha-label", State: db.PRStateOpen, IsAssigned: true,
			EffectiveReviewID: &reviewID, EffectiveReviewState: test.state,
		})
		if err != nil {
			t.Fatal(err)
		}
		pr, err := d.GetPR(prID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := server.currentReviewLabel(*pr)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Errorf("state %q label = %q, want %q", test.state, got, test.want)
		}
	}
}
