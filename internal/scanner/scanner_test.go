package scanner

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
	gh "github.com/sephriot/code-reviewer/internal/github"
)

type fakeGH struct {
	details     map[string]*gh.PRSummary
	hasReviewed map[string]bool
	getCalls    int
}

func (f *fakeGH) key(owner, repo string, n int) string {
	return prKey(owner, repo, n)
}

func (f *fakeGH) ListAssignedPRs(ctx context.Context) ([]gh.PRSummary, error) {
	return nil, nil
}
func (f *fakeGH) ListOwnPRs(ctx context.Context) ([]gh.PRSummary, error) {
	return nil, nil
}
func (f *fakeGH) GetPRDetails(ctx context.Context, owner, repo string, number int) (*gh.PRSummary, error) {
	f.getCalls++
	d := f.details[f.key(owner, repo, number)]
	if d == nil {
		return nil, context.Canceled
	}
	cp := *d
	return &cp, nil
}
func (f *fakeGH) HasUserReviewed(ctx context.Context, owner, repo string, number int) (bool, error) {
	return f.hasReviewed[f.key(owner, repo, number)], nil
}

func testScanner(t *testing.T, cfg *config.Config, fake *fakeGH) (*Scanner, *db.DB) {
	t.Helper()
	log.SetOutput(io.Discard)
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if cfg == nil {
		cfg = &config.Config{}
	}
	s := &Scanner{cfg: cfg, gh: fake, db: d}
	return s, d
}

func TestProcessPR_RepoFilterUpsertsFiltered(t *testing.T) {
	fake := &fakeGH{details: map[string]*gh.PRSummary{}}
	s, d := testScanner(t, &config.Config{Repositories: []string{`^spacelift-io/backend$`}}, fake)

	created, err := s.processPR(context.Background(), gh.PRSummary{
		Owner: "other", Repo: "thing", Number: 1, Title: "nope", Author: "a", CommitSHA: "c1", State: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("expected no review request")
	}
	if fake.getCalls != 0 {
		t.Fatalf("repo filter should not call GetPRDetails, got %d", fake.getCalls)
	}
	pr, err := d.GetPRByRepoAndNumber("other/thing", 1)
	if err != nil || pr == nil {
		t.Fatalf("expected upserted PR: %v %#v", err, pr)
	}
	if pr.FilteredReason != "repo" {
		t.Fatalf("FilteredReason=%q want repo", pr.FilteredReason)
	}
	filtered, _ := d.ListFilteredPRs()
	if len(filtered) != 1 {
		t.Fatalf("filtered len=%d", len(filtered))
	}
}

func TestProcessPR_AuthorFilterUpsertsFiltered(t *testing.T) {
	fake := &fakeGH{}
	s, d := testScanner(t, &config.Config{PRAuthors: []string{`^alice$`}}, fake)

	_, err := s.processPR(context.Background(), gh.PRSummary{
		Owner: "spacelift-io", Repo: "backend", Number: 2, Title: "x", Author: "bob", CommitSHA: "c2", State: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := d.GetPRByRepoAndNumber("spacelift-io/backend", 2)
	if pr == nil || pr.FilteredReason != "author" {
		t.Fatalf("got %#v", pr)
	}
}

func TestProcessPR_DraftFiltered(t *testing.T) {
	key := prKey("spacelift-io", "backend", 3)
	fake := &fakeGH{details: map[string]*gh.PRSummary{
		key: {Owner: "spacelift-io", Repo: "backend", Number: 3, Title: "d", Author: "a", CommitSHA: "c3", Draft: true, State: "open"},
	}}
	s, d := testScanner(t, &config.Config{}, fake)
	_, err := s.processPR(context.Background(), gh.PRSummary{
		Owner: "spacelift-io", Repo: "backend", Number: 3, Title: "d", Author: "a", CommitSHA: "c3",
	})
	if err != nil {
		t.Fatal(err)
	}
	pr, _ := d.GetPRByRepoAndNumber("spacelift-io/backend", 3)
	if pr == nil || pr.FilteredReason != "draft" || pr.State != "open" {
		t.Fatalf("got %#v", pr)
	}
	hist, _ := d.ListHistoryPRs()
	for _, h := range hist {
		if h.ID == pr.ID {
			t.Fatal("draft should not be in history")
		}
	}
}

func TestProcessPR_ClosedGoesToHistoryNotFiltered(t *testing.T) {
	key := prKey("spacelift-io", "backend", 15571)
	fake := &fakeGH{details: map[string]*gh.PRSummary{
		key: {Owner: "spacelift-io", Repo: "backend", Number: 15571, Title: "merged", Author: "kutluhanmetin", CommitSHA: "dead", State: "closed"},
	}}
	s, d := testScanner(t, &config.Config{}, fake)
	created, err := s.processPR(context.Background(), gh.PRSummary{
		Owner: "spacelift-io", Repo: "backend", Number: 15571, Title: "merged", Author: "kutluhanmetin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("no review request for closed")
	}
	pr, _ := d.GetPRByRepoAndNumber("spacelift-io/backend", 15571)
	if pr == nil || pr.State != "closed" || pr.FilteredReason != "" || pr.NeedsReview {
		t.Fatalf("got %#v", pr)
	}
	filtered, _ := d.ListFilteredPRs()
	history, _ := d.ListHistoryPRs()
	for _, f := range filtered {
		if f.ID == pr.ID {
			t.Fatal("closed PR on filtered")
		}
	}
	found := false
	for _, h := range history {
		if h.ID == pr.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("closed PR missing from history")
	}
}

func TestProcessPR_MergedGoesToHistoryAsMerged(t *testing.T) {
	key := prKey("spacelift-io", "backend", 15572)
	fake := &fakeGH{details: map[string]*gh.PRSummary{
		key: {Owner: "spacelift-io", Repo: "backend", Number: 15572, Title: "shipped", Author: "kutluhanmetin", CommitSHA: "beef", State: "merged"},
	}}
	s, d := testScanner(t, &config.Config{}, fake)
	created, err := s.processPR(context.Background(), gh.PRSummary{
		Owner: "spacelift-io", Repo: "backend", Number: 15572, Title: "shipped", Author: "kutluhanmetin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("no review request for merged")
	}
	pr, _ := d.GetPRByRepoAndNumber("spacelift-io/backend", 15572)
	if pr == nil || pr.State != "merged" || pr.FilteredReason != "" || pr.NeedsReview {
		t.Fatalf("got %#v", pr)
	}
	history, _ := d.ListHistoryPRs()
	found := false
	for _, h := range history {
		if h.ID == pr.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("merged PR missing from history")
	}
}

func TestProcessPR_DraftToReadySameSHAClearsFilter(t *testing.T) {
	key := prKey("spacelift-io", "backend", 10)
	sha := "same-sha"
	fake := &fakeGH{
		details: map[string]*gh.PRSummary{
			key: {Owner: "spacelift-io", Repo: "backend", Number: 10, Title: "ready", Author: "a", CommitSHA: sha, Draft: false, State: "open"},
		},
		hasReviewed: map[string]bool{key: false},
	}
	s, d := testScanner(t, &config.Config{}, fake)
	_, err := d.UpsertPR(db.PullRequest{
		Repo: "spacelift-io/backend", PRNumber: 10, Title: "ready", Author: "a",
		CommitSHA: sha, Draft: true, State: "open", NeedsReview: false, FilteredReason: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}

	created, err := s.processPR(context.Background(), gh.PRSummary{
		Owner: "spacelift-io", Repo: "backend", Number: 10, Title: "ready", Author: "a", CommitSHA: sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("same SHA path should not create review request")
	}
	pr, _ := d.GetPRByRepoAndNumber("spacelift-io/backend", 10)
	if pr.FilteredReason != "" {
		t.Fatalf("expected cleared filter, got %q", pr.FilteredReason)
	}
	if !pr.NeedsReview {
		t.Fatal("expected needs_review=true")
	}
	dash, _ := d.ListPRsNeedingReview()
	found := false
	for _, p := range dash {
		if p.ID == pr.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("expected on dashboard after draft→ready")
	}
}

func TestReconcileStale_FilteredThenClosedLeavesFiltered(t *testing.T) {
	key := prKey("spacelift-io", "backend", 20)
	fake := &fakeGH{details: map[string]*gh.PRSummary{
		key: {Owner: "spacelift-io", Repo: "backend", Number: 20, Title: "gone", Author: "a", CommitSHA: "z", State: "closed"},
	}}
	s, d := testScanner(t, &config.Config{}, fake)
	id, err := d.UpsertPR(db.PullRequest{
		Repo: "spacelift-io/backend", PRNumber: 20, Title: "gone", Author: "a",
		CommitSHA: "z", State: "open", FilteredReason: "draft", NeedsReview: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	s.reconcileStalePRs(context.Background(), map[string]gh.PRSummary{})

	pr, _ := d.GetPR(id)
	if pr.State != "closed" || pr.FilteredReason != "" {
		t.Fatalf("got %#v", pr)
	}
	filtered, _ := d.ListFilteredPRs()
	for _, f := range filtered {
		if f.ID == id {
			t.Fatal("should not remain on filtered")
		}
	}
	history, _ := d.ListHistoryPRs()
	found := false
	for _, h := range history {
		if h.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("should be on history")
	}
}

func TestBackfillMergedStates_UpgradesClosedToMerged(t *testing.T) {
	keyMerged := prKey("spacelift-io", "backend", 30)
	keyClosed := prKey("spacelift-io", "backend", 31)
	fake := &fakeGH{details: map[string]*gh.PRSummary{
		keyMerged: {Owner: "spacelift-io", Repo: "backend", Number: 30, Title: "shipped", Author: "a", CommitSHA: "a1", State: "merged"},
		keyClosed: {Owner: "spacelift-io", Repo: "backend", Number: 31, Title: "abandoned", Author: "b", CommitSHA: "b1", State: "closed"},
	}}
	s, d := testScanner(t, &config.Config{}, fake)
	if _, err := d.UpsertPR(db.PullRequest{
		Repo: "spacelift-io/backend", PRNumber: 30, Title: "shipped", Author: "a",
		CommitSHA: "a1", State: "closed", NeedsReview: false,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UpsertPR(db.PullRequest{
		Repo: "spacelift-io/backend", PRNumber: 31, Title: "abandoned", Author: "b",
		CommitSHA: "b1", State: "closed", NeedsReview: false,
	}); err != nil {
		t.Fatal(err)
	}

	s.backfillMergedStates(context.Background())

	merged, err := d.GetPRByRepoAndNumber("spacelift-io/backend", 30)
	if err != nil || merged == nil || merged.State != "merged" {
		t.Fatalf("want merged, got %#v err=%v", merged, err)
	}
	closed, err := d.GetPRByRepoAndNumber("spacelift-io/backend", 31)
	if err != nil || closed == nil || closed.State != "closed" {
		t.Fatalf("want still closed, got %#v err=%v", closed, err)
	}
}
