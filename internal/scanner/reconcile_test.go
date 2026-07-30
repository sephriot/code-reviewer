package scanner

import (
	"reflect"
	"testing"
)

func TestDecideReconciliation(t *testing.T) {
	tests := []struct {
		name string
		in   ReconcileInput
		want ReconcileDecision
	}{
		{
			name: "assigned open PR enters dashboard and queues current head",
			in: ReconcileInput{
				AssignedInSnapshot: true,
				SnapshotComplete:   true,
				State:              "open",
				HeadSHA:            "sha-1",
			},
			want: ReconcileDecision{
				Placement:  PlacementDashboard,
				IsAssigned: true,
				CreateSHA:  "sha-1",
			},
		},
		{
			name: "reviewed assigned PR stays on dashboard without queueing",
			in: ReconcileInput{
				AssignedInSnapshot: true,
				SnapshotComplete:   true,
				State:              "open",
				HeadSHA:            "sha-2",
				EffectiveReview:    &EffectiveReview{ID: 42, State: EffectiveReviewApproved},
			},
			want: ReconcileDecision{
				Placement:  PlacementDashboard,
				IsAssigned: true,
			},
		},
		{
			name: "complete snapshot absence moves open PR to history",
			in: ReconcileInput{
				SnapshotComplete: true,
				ExistingAssigned: true,
				State:            "open",
				HeadSHA:          "sha-3",
			},
			want: ReconcileDecision{
				Placement: PlacementHistory,
			},
		},
		{
			name: "incomplete snapshot absence preserves assignment and active request",
			in: ReconcileInput{
				ExistingAssigned: true,
				State:            "open",
				HeadSHA:          "sha-4",
				Requests: []RequestFact{
					{ID: 7, CommitSHA: "sha-4", Status: RequestPending},
				},
			},
			want: ReconcileDecision{
				Placement:  PlacementDashboard,
				IsAssigned: true,
			},
		},
		{
			name: "filter moves assigned PR and cancels current request",
			in: ReconcileInput{
				AssignedInSnapshot: true,
				SnapshotComplete:   true,
				State:              "open",
				HeadSHA:            "sha-5",
				FilteredReason:     "author",
				Requests: []RequestFact{
					{ID: 8, CommitSHA: "sha-5", Status: RequestInProgress},
				},
			},
			want: ReconcileDecision{
				Placement:      PlacementFiltered,
				IsAssigned:     true,
				FilteredReason: "author",
				Cancel: []RequestCancellation{
					{ID: 8, Status: RequestCanceled},
				},
			},
		},
		{
			name: "new head supersedes old request and queues once",
			in: ReconcileInput{
				AssignedInSnapshot: true,
				SnapshotComplete:   true,
				State:              "open",
				HeadSHA:            "sha-new",
				Requests: []RequestFact{
					{ID: 9, CommitSHA: "sha-old", Status: RequestPending},
				},
			},
			want: ReconcileDecision{
				Placement:  PlacementDashboard,
				IsAssigned: true,
				Cancel: []RequestCancellation{
					{ID: 9, Status: RequestSuperseded},
				},
				CreateSHA: "sha-new",
			},
		},
		{
			name: "completed local review blocks current head",
			in: ReconcileInput{
				AssignedInSnapshot:      true,
				SnapshotComplete:        true,
				State:                   "open",
				HeadSHA:                 "sha-6",
				HasCompletedLocalReview: true,
			},
			want: ReconcileDecision{
				Placement:  PlacementDashboard,
				IsAssigned: true,
			},
		},
		{
			name: "user suppression blocks automatic same-head retry",
			in: ReconcileInput{
				AssignedInSnapshot: true,
				SnapshotComplete:   true,
				State:              "open",
				HeadSHA:            "sha-7",
				Requests: []RequestFact{
					{ID: 10, CommitSHA: "sha-7", Status: RequestSuppressed},
				},
			},
			want: ReconcileDecision{
				Placement:  PlacementDashboard,
				IsAssigned: true,
			},
		},
		{
			name: "failed request blocks automatic same-head retry",
			in: ReconcileInput{
				AssignedInSnapshot: true,
				SnapshotComplete:   true,
				State:              "open",
				HeadSHA:            "sha-8",
				Requests: []RequestFact{
					{ID: 11, CommitSHA: "sha-8", Status: RequestFailed},
				},
			},
			want: ReconcileDecision{
				Placement:  PlacementDashboard,
				IsAssigned: true,
			},
		},
		{
			name: "new head ignores terminal stop from old head",
			in: ReconcileInput{
				AssignedInSnapshot: true,
				SnapshotComplete:   true,
				State:              "open",
				HeadSHA:            "sha-10",
				Requests: []RequestFact{
					{ID: 12, CommitSHA: "sha-9", Status: RequestFailed},
				},
			},
			want: ReconcileDecision{
				Placement:  PlacementDashboard,
				IsAssigned: true,
				CreateSHA:  "sha-10",
			},
		},
		{
			name: "draft is filtered",
			in: ReconcileInput{
				AssignedInSnapshot: true,
				SnapshotComplete:   true,
				State:              "open",
				HeadSHA:            "sha-11",
				Draft:              true,
			},
			want: ReconcileDecision{
				Placement:      PlacementFiltered,
				IsAssigned:     true,
				FilteredReason: "draft",
			},
		},
		{
			name: "manual pending preserved after completed local review",
			in: ReconcileInput{
				AssignedInSnapshot:      true,
				SnapshotComplete:        true,
				State:                   "open",
				HeadSHA:                 "sha-12",
				HasCompletedLocalReview: true,
				Requests: []RequestFact{
					{ID: 13, CommitSHA: "sha-12", Status: RequestPending},
				},
			},
			want: ReconcileDecision{
				Placement:  PlacementDashboard,
				IsAssigned: true,
			},
		},
		{
			name: "effective github review cancels manual pending",
			in: ReconcileInput{
				AssignedInSnapshot:      true,
				SnapshotComplete:        true,
				State:                   "open",
				HeadSHA:                 "sha-13",
				HasCompletedLocalReview: true,
				EffectiveReview:         &EffectiveReview{ID: 99, State: EffectiveReviewApproved},
				Requests: []RequestFact{
					{ID: 14, CommitSHA: "sha-13", Status: RequestPending},
				},
			},
			want: ReconcileDecision{
				Placement:  PlacementDashboard,
				IsAssigned: true,
				Cancel: []RequestCancellation{
					{ID: 14, Status: RequestCanceled},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideReconciliation(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("DecideReconciliation() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
