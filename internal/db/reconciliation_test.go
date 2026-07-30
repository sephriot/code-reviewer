package db

import "testing"

func TestReconciliationFieldsRoundTrip(t *testing.T) {
	d := openTestDB(t)
	reviewID := int64(44)
	prID := mustUpsert(t, d, PullRequest{
		Repo:                 "acme/repo",
		PRNumber:             1,
		Title:                "review state",
		Author:               "alice",
		CommitSHA:            "sha-1",
		State:                PRStateOpen,
		IsAssigned:           true,
		EffectiveReviewID:    &reviewID,
		EffectiveReviewState: EffectiveReviewStateApproved,
	})

	pr, err := d.GetPR(prID)
	if err != nil {
		t.Fatal(err)
	}
	if !pr.IsAssigned || pr.EffectiveReviewID == nil || *pr.EffectiveReviewID != 44 {
		t.Fatalf("assignment/review ID did not round-trip: %#v", pr)
	}
	if pr.EffectiveReviewState != EffectiveReviewStateApproved {
		t.Fatalf("effective state = %q, want approved", pr.EffectiveReviewState)
	}

	requestID, err := d.CreateReviewRequest(prID, "sha-1")
	if err != nil {
		t.Fatal(err)
	}
	localID, err := d.CreateReview(Review{
		PullRequestID:   prID,
		ReviewRequestID: requestID,
		Outcome:         ReviewOutcomeApproveWithoutComments,
		CommitSHA:       "sha-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PublishReview(localID, 44); err != nil {
		t.Fatal(err)
	}
	local, err := d.GetReviewByGitHubID(44)
	if err != nil {
		t.Fatal(err)
	}
	if local == nil || local.ID != localID || local.GitHubReviewID == nil || *local.GitHubReviewID != 44 {
		t.Fatalf("published review = %#v, want local review %d with GitHub ID", local, localID)
	}
}

func TestActiveRequestUniquePerPRSHA(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "acme/repo", PRNumber: 2, Title: "queue", Author: "alice",
		CommitSHA: "sha-2", State: PRStateOpen, IsAssigned: true,
	})

	if _, err := d.CreateReviewRequest(prID, "sha-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateReviewRequest(prID, "sha-2"); err == nil {
		t.Fatal("duplicate active request for one PR/SHA must fail")
	}
	if err := d.SetReviewRequestStatusForSHA(prID, "sha-2", ReviewRequestStatusFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateReviewRequest(prID, "sha-2"); err != nil {
		t.Fatalf("terminal request must allow explicit retry: %v", err)
	}
}

func TestPRPlacementQueriesUseAssignmentAndFilterState(t *testing.T) {
	d := openTestDB(t)
	reviewID := int64(9)
	dashboardID := mustUpsert(t, d, PullRequest{
		Repo: "acme/a", PRNumber: 1, Title: "reviewed", Author: "alice",
		CommitSHA: "a", State: PRStateOpen, IsAssigned: true,
		EffectiveReviewID: &reviewID, EffectiveReviewState: EffectiveReviewStateApproved,
	})
	filteredID := mustUpsert(t, d, PullRequest{
		Repo: "acme/b", PRNumber: 2, Title: "draft", Author: "bob",
		CommitSHA: "b", State: PRStateOpen, IsAssigned: true, FilteredReason: "draft",
	})
	historyID := mustUpsert(t, d, PullRequest{
		Repo: "acme/c", PRNumber: 3, Title: "unassigned", Author: "carol",
		CommitSHA: "c", State: PRStateOpen,
	})

	dashboard, err := d.ListDashboardPRs()
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := d.ListFilteredPRs()
	if err != nil {
		t.Fatal(err)
	}
	history, err := d.ListHistoryPRs()
	if err != nil {
		t.Fatal(err)
	}
	if !idsOf(dashboard)[dashboardID] || idsOf(filtered)[dashboardID] || idsOf(history)[dashboardID] {
		t.Fatalf("reviewed assigned PR placement is wrong")
	}
	if !idsOf(filtered)[filteredID] || idsOf(dashboard)[filteredID] || idsOf(history)[filteredID] {
		t.Fatalf("filtered assigned PR placement is wrong")
	}
	if !idsOf(history)[historyID] || idsOf(dashboard)[historyID] || idsOf(filtered)[historyID] {
		t.Fatalf("unassigned PR placement is wrong")
	}
}

func TestMigrateCancelsTargetlessActiveRequestWithoutDeletingReview(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "acme/repo", PRNumber: 4, Title: "legacy", Author: "alice",
		CommitSHA: "sha-4", State: PRStateOpen, IsAssigned: true,
	})
	requestID, err := d.CreateReviewRequest(prID, "sha-4")
	if err != nil {
		t.Fatal(err)
	}
	reviewID, err := d.CreateReview(Review{
		PullRequestID: prID, ReviewRequestID: requestID,
		Outcome: ReviewOutcomeHumanReview, CommitSHA: "sha-4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec("UPDATE review_requests SET commit_sha = '' WHERE id = ?", requestID); err != nil {
		t.Fatal(err)
	}
	if err := migrate(d.DB); err != nil {
		t.Fatal(err)
	}

	request, err := d.GetReviewRequestIncludingTerminal(requestID)
	if err != nil {
		t.Fatal(err)
	}
	if request == nil || request.Status != ReviewRequestStatusCanceled {
		t.Fatalf("targetless request = %#v, want canceled", request)
	}
	review, err := d.GetReview(reviewID)
	if err != nil {
		t.Fatal(err)
	}
	if review == nil {
		t.Fatal("migration must preserve review history")
	}
	if _, err := d.CreateReviewRequest(prID, "sha-4"); err != nil {
		t.Fatalf("new targeted request after migration: %v", err)
	}
}

func TestApplyReconciliationUpdatesFactsAndQueueAtomically(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "acme/repo", PRNumber: 5, Title: "old", Author: "alice",
		CommitSHA: "sha-old", State: PRStateOpen, IsAssigned: true,
	})
	oldRequestID, err := d.CreateReviewRequest(prID, "sha-old")
	if err != nil {
		t.Fatal(err)
	}
	reviewID := int64(55)

	result, err := d.ApplyReconciliation(ReconciliationChange{
		PR: PullRequest{
			Repo: "acme/repo", PRNumber: 5, Title: "new", Author: "alice",
			CommitSHA: "sha-new", State: PRStateOpen, IsAssigned: true,
			EffectiveReviewID: &reviewID, EffectiveReviewState: EffectiveReviewStateCommented,
		},
		Cancel: []RequestStatusChange{
			{ID: oldRequestID, Status: ReviewRequestStatusSuperseded},
		},
		CreateSHA: "sha-new",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.PullRequestID != prID || result.CreatedRequestID == 0 {
		t.Fatalf("apply result = %#v", result)
	}
	pr, err := d.GetPR(prID)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Title != "new" || pr.CommitSHA != "sha-new" || pr.EffectiveReviewID == nil || *pr.EffectiveReviewID != 55 {
		t.Fatalf("updated PR = %#v", pr)
	}
	oldRequest, err := d.GetReviewRequestIncludingTerminal(oldRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if oldRequest.Status != ReviewRequestStatusSuperseded {
		t.Fatalf("old request status = %q, want superseded", oldRequest.Status)
	}
	newRequest, err := d.GetReviewRequest(result.CreatedRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if newRequest == nil || newRequest.CommitSHA != "sha-new" {
		t.Fatalf("new request = %#v", newRequest)
	}
}

func TestApplyReconciliationRollsBackPRWhenQueueDecisionIsInvalid(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "acme/repo", PRNumber: 6, Title: "stable", Author: "alice",
		CommitSHA: "sha-6", State: PRStateOpen, IsAssigned: true,
	})

	_, err := d.ApplyReconciliation(ReconciliationChange{
		PR: PullRequest{
			Repo: "acme/repo", PRNumber: 6, Title: "must roll back", Author: "alice",
			CommitSHA: "sha-7", State: PRStateOpen, IsAssigned: true,
		},
		Cancel: []RequestStatusChange{
			{ID: 99999, Status: ReviewRequestStatusCanceled},
		},
	})
	if err == nil {
		t.Fatal("missing request cancellation must fail the transaction")
	}
	pr, getErr := d.GetPR(prID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if pr.Title != "stable" || pr.CommitSHA != "sha-6" {
		t.Fatalf("PR update escaped rollback: %#v", pr)
	}
}

func TestCompletedLocalReviewForSHADistinguishesRunnerSuccess(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "acme/repo", PRNumber: 7, Title: "review history", Author: "alice",
		CommitSHA: "sha-7", State: PRStateOpen, IsAssigned: true,
	})

	outcomes := []string{
		ReviewOutcomeToolFailed,
		ReviewOutcomeReviewedExternally,
		ReviewOutcomeApproveWithComments,
	}
	for i, outcome := range outcomes {
		requestID, err := d.CreateReviewRequest(prID, "sha-"+string(rune('7'+i)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.CreateReview(Review{
			PullRequestID: prID, ReviewRequestID: requestID,
			Outcome: outcome, CommitSHA: "sha-7",
		}); err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateReviewRequestStatus(requestID, ReviewRequestStatusDone); err != nil {
			t.Fatal(err)
		}
	}

	completed, err := d.HasCompletedReviewForSHA(prID, "sha-7")
	if err != nil {
		t.Fatal(err)
	}
	if !completed {
		t.Fatal("successful runner review must count as completed")
	}
	if _, err := d.Exec("DELETE FROM reviews WHERE outcome = ?", ReviewOutcomeApproveWithComments); err != nil {
		t.Fatal(err)
	}
	completed, err = d.HasCompletedReviewForSHA(prID, "sha-7")
	if err != nil {
		t.Fatal(err)
	}
	if completed {
		t.Fatal("tool failure and legacy external rows must not count as completed local review")
	}
}

func TestListReviewRequestsForPRIncludesTerminalStops(t *testing.T) {
	d := openTestDB(t)
	prID := mustUpsert(t, d, PullRequest{
		Repo: "acme/repo", PRNumber: 8, Title: "request history", Author: "alice",
		CommitSHA: "sha-8", State: PRStateOpen, IsAssigned: true,
	})
	requestID, err := d.CreateReviewRequest(prID, "sha-8")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateReviewRequestStatus(requestID, ReviewRequestStatusSuppressed); err != nil {
		t.Fatal(err)
	}

	requests, err := d.ListReviewRequestsForPR(prID)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].Status != ReviewRequestStatusSuppressed || requests[0].CommitSHA != "sha-8" {
		t.Fatalf("request history = %#v", requests)
	}
}

func TestManualRetryAllowsFailedOrSuppressedCurrentSHA(t *testing.T) {
	for _, terminalStatus := range []string{
		ReviewRequestStatusFailed,
		ReviewRequestStatusSuppressed,
	} {
		t.Run(terminalStatus, func(t *testing.T) {
			d := openTestDB(t)
			prID := mustUpsert(t, d, PullRequest{
				Repo: "acme/repo", PRNumber: 30, Title: "retry", Author: "alice",
				CommitSHA: "sha-retry", State: PRStateOpen, IsAssigned: true,
			})
			requestID, err := d.CreateReviewRequest(prID, "sha-retry")
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateReviewRequestStatus(requestID, terminalStatus); err != nil {
				t.Fatal(err)
			}

			retryID, err := d.CreateManualReviewRequest(prID)
			if err != nil {
				t.Fatal(err)
			}
			retry, err := d.GetReviewRequest(retryID)
			if err != nil {
				t.Fatal(err)
			}
			if retry == nil || retry.CommitSHA != "sha-retry" || retry.Status != ReviewRequestStatusPending {
				t.Fatalf("manual retry = %#v", retry)
			}
		})
	}
}
