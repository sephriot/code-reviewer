package db

import "time"

type PullRequest struct {
	ID                   int64      `json:"id"`
	Repo                 string     `json:"repo"`
	PRNumber             int        `json:"pr_number"`
	Title                string     `json:"title"`
	Author               string     `json:"author"`
	CommitSHA            string     `json:"commit_sha"`
	Draft                bool       `json:"draft"`
	State                string     `json:"state"`
	NeedsReview          bool       `json:"needs_review"`
	IsOutdated           bool       `json:"is_outdated"`
	FilteredReason       string     `json:"filtered_reason,omitempty"`
	GhUpdatedAt          time.Time  `json:"gh_updated_at,omitempty"`
	IsAssigned           bool       `json:"is_assigned"`
	EffectiveReviewID    *int64     `json:"effective_review_id,omitempty"`
	EffectiveReviewState string     `json:"effective_review_state,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	DeletedAt            *time.Time `json:"deleted_at,omitempty"`
}

type ReviewRequest struct {
	ID            int64      `json:"id"`
	PullRequestID int64      `json:"pull_request_id"`
	Status        string     `json:"status"`
	CommitSHA     string     `json:"commit_sha"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}

type Review struct {
	ID              int64      `json:"id"`
	PullRequestID   int64      `json:"pull_request_id"`
	ReviewRequestID int64      `json:"review_request_id"`
	Outcome         string     `json:"outcome"`
	CommitSHA       string     `json:"commit_sha"`
	Summary         string     `json:"summary"`
	GeneralComment  string     `json:"general_comment"`
	Published       bool       `json:"published"`
	GitHubReviewID  *int64     `json:"github_review_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	DeletedAt       *time.Time `json:"deleted_at,omitempty"`
}

type ReviewComment struct {
	ID        int64      `json:"id"`
	ReviewID  int64      `json:"review_id"`
	File      string     `json:"file"`
	Line      int        `json:"line"`
	Message   string     `json:"message"`
	Published bool       `json:"published"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

type PublishedReviewView struct {
	Review
	Repo     string `json:"repo"`
	PRNumber int    `json:"pr_number"`
	PRTitle  string `json:"pr_title"`
	PRAuthor string `json:"pr_author"`
}

const (
	ReviewOutcomeApproveWithoutComments = "approve_without_comments"
	ReviewOutcomeApproveWithComments    = "approve_with_comments"
	ReviewOutcomeChangesRequested       = "changes_requested"
	ReviewOutcomeHumanReview            = "human_review"
	ReviewOutcomeToolFailed             = "tool_failed"
	ReviewOutcomeReviewedExternally     = "reviewed_externally"
)

const (
	EffectiveReviewStateCommented        = "commented"
	EffectiveReviewStateApproved         = "approved"
	EffectiveReviewStateChangesRequested = "changes_requested"
)

const (
	ReviewRequestStatusPending    = "pending"
	ReviewRequestStatusInProgress = "in_progress"
	ReviewRequestStatusDone       = "done"
	ReviewRequestStatusFailed     = "failed"
	ReviewRequestStatusCanceled   = "canceled"
	ReviewRequestStatusSuperseded = "superseded"
	ReviewRequestStatusSuppressed = "suppressed"
)

const (
	PRStateOpen   = "open"
	PRStateClosed = "closed"
	PRStateMerged = "merged"
)

// Trend bucket granularities for ReviewsByOutcomeOverTime.
const (
	TrendBucketDay  = "day"
	TrendBucketWeek = "week"
)

// OutcomeCountRow is one sparse GROUP BY row from ReviewsByOutcomeOverTime.
type OutcomeCountRow struct {
	Bucket  string
	Outcome string
	Count   int
}

// TrendBucket is a zero-filled analytics time bucket for the API/UI.
type TrendBucket struct {
	Date     string         `json:"date,omitempty"`
	Week     string         `json:"week,omitempty"`
	Total    int            `json:"total"`
	Outcomes map[string]int `json:"outcomes"`
}

// AuthorStats is per-author review analytics for the selected period.
type AuthorStats struct {
	Author            string  `json:"author"`
	TotalReviews      int     `json:"total_reviews"`
	ApprovalRate      float64 `json:"approval_rate"`
	HumanReviewRate   float64 `json:"human_review_rate"`
	ChangeRequestRate float64 `json:"change_request_rate"`
	AvgInlineComments float64 `json:"avg_inline_comments"`
}
