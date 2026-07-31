package scanner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
	gh "github.com/sephriot/code-reviewer/internal/github"
)

type fakeGH struct {
	snapshot     gh.AssignmentSnapshot
	discoveryErr error
	details      map[string]*gh.PRSummary
	reviews      map[string]*gh.EffectiveReview
	reviewErrors map[string]error
}

func (f *fakeGH) ListReviewAssignments(context.Context) (gh.AssignmentSnapshot, error) {
	return f.snapshot, f.discoveryErr
}

func (f *fakeGH) GetPRDetails(_ context.Context, owner, repo string, number int) (*gh.PRSummary, error) {
	detail := f.details[prKey(owner, repo, number)]
	if detail == nil {
		return nil, errors.New("missing PR detail")
	}
	copy := *detail
	return &copy, nil
}

func (f *fakeGH) GetEffectiveReview(_ context.Context, owner, repo string, number int) (*gh.EffectiveReview, error) {
	key := prKey(owner, repo, number)
	if err := f.reviewErrors[key]; err != nil {
		return nil, err
	}
	return f.reviews[key], nil
}

type recordingCanceller struct {
	db       *db.DB
	canceled []int64
	t        *testing.T
}

func (c *recordingCanceller) CancelSystemRequest(id int64) {
	c.t.Helper()
	request, err := c.db.GetReviewRequestIncludingTerminal(id)
	if err != nil {
		c.t.Fatal(err)
	}
	if request == nil || (request.Status != db.ReviewRequestStatusCanceled && request.Status != db.ReviewRequestStatusSuperseded) {
		c.t.Fatalf("cancellation ran before terminal state committed: %#v", request)
	}
	c.canceled = append(c.canceled, id)
}

func testScanner(t *testing.T, cfg *config.Config, fake *fakeGH, canceller queueCanceller, onNew func()) (*Scanner, *db.DB) {
	t.Helper()
	log.SetOutput(io.Discard)
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if cfg == nil {
		cfg = &config.Config{}
	}
	return &Scanner{cfg: cfg, gh: fake, db: database, canceller: canceller, onNew: onNew}, database
}

func TestScanIncompleteSnapshotPreservesAssignment(t *testing.T) {
	key := prKey("acme", "repo", 1)
	fake := &fakeGH{
		snapshot:     gh.AssignmentSnapshot{Complete: false},
		discoveryErr: errors.New("team page failed"),
		details: map[string]*gh.PRSummary{
			key: {Owner: "acme", Repo: "repo", Number: 1, Title: "tracked", Author: "alice", CommitSHA: "sha-1", State: "open"},
		},
		reviews: map[string]*gh.EffectiveReview{},
	}
	scanner, database := testScanner(t, nil, fake, nil, nil)
	prID, err := database.UpsertPR(db.PullRequest{
		Repo: "acme/repo", PRNumber: 1, Title: "tracked", Author: "alice",
		CommitSHA: "sha-1", State: db.PRStateOpen, IsAssigned: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	if err := scanner.Scan(context.Background()); err == nil {
		t.Fatal("partial discovery error must remain visible")
	}
	pr, err := database.GetPR(prID)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.IsAssigned {
		t.Fatal("incomplete snapshot must preserve assignment")
	}
	dashboard, err := database.ListDashboardPRs()
	if !idsOfPRs(t, dashboard, err)[prID] {
		t.Fatal("preserved assigned PR must remain on Dashboard")
	}
	output := logs.String()
	if !strings.Contains(output, "scan: discovered assigned=0 tracked_open=1 candidates=1 complete=false") {
		t.Fatalf("incomplete discovery log missing:\n%s", output)
	}
	if !strings.Contains(output, "scan: done candidates=1 reconciled=1 failed=1 created=1 canceled=0 superseded=0 complete=false duration=") {
		t.Fatalf("incomplete summary log missing:\n%s", output)
	}
}

func TestScanRetainedEffectiveReviewKeepsNewHeadOnDashboardWithoutQueue(t *testing.T) {
	key := prKey("acme", "repo", 2)
	fake := &fakeGH{
		snapshot: gh.AssignmentSnapshot{
			Complete: true,
			PRs:      []gh.PRSummary{{Owner: "acme", Repo: "repo", Number: 2}},
		},
		details: map[string]*gh.PRSummary{
			key: {Owner: "acme", Repo: "repo", Number: 2, Title: "approved", Author: "alice", CommitSHA: "sha-new", State: "open"},
		},
		reviews: map[string]*gh.EffectiveReview{
			key: {ID: 22, State: gh.ReviewStateApproved},
		},
	}
	scanner, database := testScanner(t, nil, fake, nil, nil)
	prID, err := database.UpsertPR(db.PullRequest{
		Repo: "acme/repo", PRNumber: 2, Title: "approved", Author: "alice",
		CommitSHA: "sha-old", State: db.PRStateOpen, IsAssigned: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	pr, err := database.GetPR(prID)
	if err != nil {
		t.Fatal(err)
	}
	if pr.CommitSHA != "sha-new" || pr.EffectiveReviewID == nil || *pr.EffectiveReviewID != 22 {
		t.Fatalf("reconciled PR = %#v", pr)
	}
	dashboard, err := database.ListDashboardPRs()
	if !idsOfPRs(t, dashboard, err)[prID] {
		t.Fatal("reviewed assigned PR must stay on Dashboard")
	}
	requests, err := database.ListReviewRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatalf("effective review must block queueing: %#v", requests)
	}
}

func TestScanDroppedReviewSupersedesOldHeadAndQueuesNewHeadOnce(t *testing.T) {
	key := prKey("acme", "repo", 3)
	fake := &fakeGH{
		snapshot: gh.AssignmentSnapshot{
			Complete: true,
			PRs:      []gh.PRSummary{{Owner: "acme", Repo: "repo", Number: 3}},
		},
		details: map[string]*gh.PRSummary{
			key: {Owner: "acme", Repo: "repo", Number: 3, Title: "updated", Author: "alice", CommitSHA: "sha-new", State: "open"},
		},
		reviews: map[string]*gh.EffectiveReview{},
	}
	wakeCount := 0
	scanner, database := testScanner(t, nil, fake, nil, func() { wakeCount++ })
	prID, err := database.UpsertPR(db.PullRequest{
		Repo: "acme/repo", PRNumber: 3, Title: "updated", Author: "alice",
		CommitSHA: "sha-old", State: db.PRStateOpen, IsAssigned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	oldRequestID, err := database.CreateReviewRequest(prID, "sha-old")
	if err != nil {
		t.Fatal(err)
	}
	canceller := &recordingCanceller{db: database, t: t}
	scanner.canceller = canceller

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var changesAfterFirstScan int64
	if err := database.QueryRow("SELECT total_changes()").Scan(&changesAfterFirstScan); err != nil {
		t.Fatal(err)
	}
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var changesAfterSecondScan int64
	if err := database.QueryRow("SELECT total_changes()").Scan(&changesAfterSecondScan); err != nil {
		t.Fatal(err)
	}
	if changesAfterSecondScan != changesAfterFirstScan {
		t.Fatalf("identical second scan wrote %d rows", changesAfterSecondScan-changesAfterFirstScan)
	}
	oldRequest, err := database.GetReviewRequestIncludingTerminal(oldRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if oldRequest.Status != db.ReviewRequestStatusSuperseded {
		t.Fatalf("old request status = %q", oldRequest.Status)
	}
	requests, err := database.ListReviewRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].CommitSHA != "sha-new" {
		t.Fatalf("active requests = %#v, want one for sha-new", requests)
	}
	if wakeCount != 1 {
		t.Fatalf("reactor wake count = %d, want 1", wakeCount)
	}
	if len(canceller.canceled) != 1 || canceller.canceled[0] != oldRequestID {
		t.Fatalf("system cancellations = %#v", canceller.canceled)
	}
}

func TestScanCompleteAbsenceMovesPRToHistoryAndKeepsOpenRequest(t *testing.T) {
	key := prKey("acme", "repo", 4)
	fake := &fakeGH{
		snapshot: gh.AssignmentSnapshot{Complete: true},
		details: map[string]*gh.PRSummary{
			key: {Owner: "acme", Repo: "repo", Number: 4, Title: "unassigned", Author: "alice", CommitSHA: "sha-4", State: "open"},
		},
		reviews: map[string]*gh.EffectiveReview{},
	}
	scanner, database := testScanner(t, nil, fake, nil, nil)
	prID, err := database.UpsertPR(db.PullRequest{
		Repo: "acme/repo", PRNumber: 4, Title: "unassigned", Author: "alice",
		CommitSHA: "sha-4", State: db.PRStateOpen, IsAssigned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := database.CreateReviewRequest(prID, "sha-4")
	if err != nil {
		t.Fatal(err)
	}
	canceller := &recordingCanceller{db: database, t: t}
	scanner.canceller = canceller

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	pr, err := database.GetPR(prID)
	if err != nil {
		t.Fatal(err)
	}
	if pr.IsAssigned {
		t.Fatal("complete absence must clear assignment")
	}
	history, err := database.ListHistoryPRs()
	if !idsOfPRs(t, history, err)[prID] {
		t.Fatal("unassigned PR must move to History")
	}
	if len(canceller.canceled) != 0 {
		t.Fatalf("open PR request must stay queued, cancellations = %#v", canceller.canceled)
	}
	request, err := database.GetReviewRequest(requestID)
	if err != nil {
		t.Fatal(err)
	}
	if request == nil || request.Status != db.ReviewRequestStatusPending {
		t.Fatalf("request = %#v, want pending", request)
	}
}

func TestScanLogsReconciliationDecisions(t *testing.T) {
	const (
		fullSHA = "0123456789abcdef0123456789abcdef01234567"
		title   = "do not log this title"
	)
	key := prKey("acme", "repo", 5)
	fake := &fakeGH{
		snapshot: gh.AssignmentSnapshot{
			Complete: true,
			PRs:      []gh.PRSummary{{Owner: "acme", Repo: "repo", Number: 5}},
		},
		details: map[string]*gh.PRSummary{
			key: {
				Owner: "acme", Repo: "repo", Number: 5, Title: title,
				Author: "alice", CommitSHA: fullSHA, State: "open",
			},
		},
		reviews: map[string]*gh.EffectiveReview{},
	}
	scanner, database := testScanner(t, nil, fake, nil, nil)
	prID, err := database.UpsertPR(db.PullRequest{
		Repo: "acme/repo", PRNumber: 5, Title: title, Author: "alice",
		CommitSHA: "old-head", State: db.PRStateOpen, IsAssigned: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateReviewRequest(prID, "old-head"); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	output := logs.String()
	if !strings.Contains(output, "scan: discovered assigned=1 tracked_open=1 candidates=1 complete=true") {
		t.Fatalf("discovery log missing:\n%s", output)
	}
	if strings.Count(output, "scan: pr=acme/repo#5 ") != 2 {
		t.Fatalf("decision log count wrong:\n%s", output)
	}
	if !strings.Contains(output, "head=0123456789ab assigned=true placement=dashboard review=none local_review=false queue=created canceled=0 superseded=1") {
		t.Fatalf("created decision log missing:\n%s", output)
	}
	if !strings.Contains(output, "head=0123456789ab assigned=true placement=dashboard review=none local_review=false queue=kept canceled=0 superseded=0") {
		t.Fatalf("kept decision log missing:\n%s", output)
	}
	if !strings.Contains(output, "scan: done candidates=1 reconciled=1 failed=0 created=1 canceled=0 superseded=1 complete=true duration=") {
		t.Fatalf("first summary log missing:\n%s", output)
	}
	if !strings.Contains(output, "scan: done candidates=1 reconciled=1 failed=0 created=0 canceled=0 superseded=0 complete=true duration=") {
		t.Fatalf("second summary log missing:\n%s", output)
	}
	if strings.Contains(output, fullSHA) || strings.Contains(output, title) {
		t.Fatalf("sensitive PR data leaked into logs:\n%s", output)
	}
}

func idsOfPRs(t *testing.T, prs []db.PullRequest, err error) map[int64]bool {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[int64]bool, len(prs))
	for _, pr := range prs {
		ids[pr.ID] = true
	}
	return ids
}
