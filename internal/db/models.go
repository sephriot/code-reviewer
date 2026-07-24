package db

import "time"

type PullRequest struct {
	ID            int64      `json:"id"`
	Repo          string     `json:"repo"`
	PRNumber      int        `json:"pr_number"`
	Title         string     `json:"title"`
	Author        string     `json:"author"`
	CommitSHA     string     `json:"commit_sha"`
	Draft         bool       `json:"draft"`
	State         string     `json:"state"`
	NeedsReview   bool       `json:"needs_review"`
	IsOutdated    bool       `json:"is_outdated"`
	FilteredReason string    `json:"filtered_reason,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}

type ReviewRequest struct {
	ID            int64      `json:"id"`
	PullRequestID int64      `json:"pull_request_id"`
	Status        string     `json:"status"`
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
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

type PublishedReviewView struct {
	Review
	Repo      string `json:"repo"`
	PRNumber  int    `json:"pr_number"`
	PRTitle   string `json:"pr_title"`
	PRAuthor  string `json:"pr_author"`
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
	ReviewRequestStatusPending    = "pending"
	ReviewRequestStatusInProgress = "in_progress"
	ReviewRequestStatusDone       = "done"
	ReviewRequestStatusFailed     = "failed"
)

const (
	PRStateOpen   = "open"
	PRStateClosed = "closed"
	PRStateMerged = "merged"
)
