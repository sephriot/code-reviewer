package scanner

type Placement string

const (
	PlacementDashboard Placement = "dashboard"
	PlacementFiltered  Placement = "filtered"
	PlacementHistory   Placement = "history"
)

type EffectiveReviewState string

const (
	EffectiveReviewCommented        EffectiveReviewState = "commented"
	EffectiveReviewApproved         EffectiveReviewState = "approved"
	EffectiveReviewChangesRequested EffectiveReviewState = "changes_requested"
)

type EffectiveReview struct {
	ID    int64
	State EffectiveReviewState
}

const (
	RequestPending    = "pending"
	RequestInProgress = "in_progress"
	RequestDone       = "done"
	RequestFailed     = "failed"
	RequestCanceled   = "canceled"
	RequestSuperseded = "superseded"
	RequestSuppressed = "suppressed"
)

type RequestFact struct {
	ID        int64
	CommitSHA string
	Status    string
}

type ReconcileInput struct {
	AssignedInSnapshot      bool
	SnapshotComplete        bool
	ExistingAssigned        bool
	State                   string
	HeadSHA                 string
	Draft                   bool
	FilteredReason          string
	EffectiveReview         *EffectiveReview
	HasCompletedLocalReview bool
	Requests                []RequestFact
}

type RequestCancellation struct {
	ID     int64
	Status string
}

type ReconcileDecision struct {
	Placement      Placement
	IsAssigned     bool
	FilteredReason string
	Cancel         []RequestCancellation
	CreateSHA      string
}

func DecideReconciliation(in ReconcileInput) ReconcileDecision {
	assigned := in.AssignedInSnapshot
	if !in.AssignedInSnapshot && !in.SnapshotComplete {
		assigned = in.ExistingAssigned
	}

	decision := ReconcileDecision{IsAssigned: assigned}
	switch {
	case in.State != "open" || !assigned:
		decision.Placement = PlacementHistory
	case in.Draft:
		decision.Placement = PlacementFiltered
		decision.FilteredReason = "draft"
	case in.FilteredReason != "":
		decision.Placement = PlacementFiltered
		decision.FilteredReason = in.FilteredReason
	default:
		decision.Placement = PlacementDashboard
	}

	autoEligible := decision.Placement == PlacementDashboard &&
		in.EffectiveReview == nil &&
		!in.HasCompletedLocalReview
	// Keep same-SHA active jobs while the PR is open (manual request from any UI list).
	keepActive := in.State == "open"
	hasActiveCurrent := false
	hasTerminalStop := false

	for _, request := range in.Requests {
		active := request.Status == RequestPending || request.Status == RequestInProgress
		if active {
			if request.CommitSHA != in.HeadSHA {
				decision.Cancel = append(decision.Cancel, RequestCancellation{
					ID:     request.ID,
					Status: RequestSuperseded,
				})
			} else if !keepActive {
				decision.Cancel = append(decision.Cancel, RequestCancellation{
					ID:     request.ID,
					Status: RequestCanceled,
				})
			} else {
				hasActiveCurrent = true
			}
		}
		if request.CommitSHA == in.HeadSHA &&
			(request.Status == RequestFailed || request.Status == RequestSuppressed) {
			hasTerminalStop = true
		}
	}

	if autoEligible && !hasActiveCurrent && !hasTerminalStop && in.HeadSHA != "" {
		decision.CreateSHA = in.HeadSHA
	}
	return decision
}
