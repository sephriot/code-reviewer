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

func TestPRRoutes(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	prID, err := d.UpsertPR(db.PullRequest{
		Repo: "spacelift-io/backend", PRNumber: 17886, Title: "Backend change",
		CommitSHA: "abc", State: db.PRStateOpen, IsAssigned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := d.CreateReviewRequest(prID, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateReview(db.Review{
		PullRequestID: prID, ReviewRequestID: requestID, CommitSHA: "abc",
		Outcome: db.ReviewOutcomeHumanReview, Summary: "Stored review feedback",
	}); err != nil {
		t.Fatal(err)
	}
	server := New(&config.Config{}, d, nil, nil)
	handler := server.handler()
	for _, path := range []string{
		"/spacelift-io/backend/pull/17886/",
		"/spacelift-io/backend/pull/17886",
		"/pr/" + strconv.FormatInt(prID, 10),
	} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", rr.Code, rr.Body.String())
			}
			for _, want := range []string{"Backend change", "Stored review feedback"} {
				if !strings.Contains(rr.Body.String(), want) {
					t.Errorf("missing %q", want)
				}
			}
		})
	}
	for _, path := range []string{
		"/other/backend/pull/17886/", "/spacelift-io/other/pull/17886/",
		"/spacelift-io/backend/pull/17887/", "/spacelift-io/backend/pull/abc/",
		"/spacelift-io/backend/pull/0/", "/spacelift-io/backend/pull/-1/",
		"/spacelift-io/backend/pull/+17886/", "/spacelift-io/backend/pull/99999999999999999999999/",
		"/spacelift-io/backend/pull/", "/spacelift-io/backend/issues/17886/",
		"/spacelift-io/backend/pull/17886/files", "/unknown",
	} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rr.Code)
			}
		})
	}
	if err := d.SoftDeletePR(prID); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/spacelift-io/backend/pull/17886/", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("deleted PR status = %d, want 404", rr.Code)
	}
}
