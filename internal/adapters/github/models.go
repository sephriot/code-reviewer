package github

import "time"

// User is the authenticated GitHub account identity.
type User struct {
	ID     int64  `json:"id"`
	NodeID string `json:"node_id"`
	Login  string `json:"login"`
}

// RateLimit is provider capacity metadata returned with every response.
type RateLimit struct {
	Limit      int           `json:"limit"`
	Remaining  int           `json:"remaining"`
	Used       int           `json:"used"`
	Resource   string        `json:"resource,omitempty"`
	Reset      time.Time     `json:"reset,omitempty"`
	RetryAfter time.Duration `json:"retry_after,omitempty"`
}

// AuthenticatedUserResult combines account identity and response capacity.
type AuthenticatedUserResult struct {
	User      User      `json:"user"`
	RateLimit RateLimit `json:"rate_limit"`
}

// SearchCandidate identifies a PR search hit without treating issue identity
// as canonical pull-request identity.
type SearchCandidate struct {
	Owner      string `json:"owner"`
	Repository string `json:"repository"`
	Number     int    `json:"number"`
}

// SearchPage is one provider search page and its completeness metadata.
type SearchPage struct {
	Candidates        []SearchCandidate `json:"candidates"`
	TotalCount        int               `json:"total_count"`
	IncompleteResults bool              `json:"incomplete_results"`
	NextPage          int               `json:"next_page,omitempty"`
	RateLimit         RateLimit         `json:"rate_limit"`
}

// Repository is the immutable target repository identity plus current name.
type Repository struct {
	ID       int64  `json:"id"`
	NodeID   string `json:"node_id"`
	FullName string `json:"full_name"`
}

// PullRequest is the authoritative normalized detail response.
type PullRequest struct {
	ID                 int64      `json:"id"`
	NodeID             string     `json:"node_id"`
	Number             int        `json:"number"`
	URL                string     `json:"url"`
	Title              string     `json:"title"`
	Body               string     `json:"body"`
	Author             User       `json:"author"`
	TargetRepository   Repository `json:"target_repository"`
	State              string     `json:"state"`
	Merged             bool       `json:"merged"`
	Draft              bool       `json:"draft"`
	HeadSHA            string     `json:"head_sha"`
	BaseSHA            string     `json:"base_sha"`
	BaseRef            string     `json:"base_ref"`
	Labels             []string   `json:"labels"`
	RequestedReviewers []User     `json:"requested_reviewers"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// PullRequestResult includes cache metadata for conditional reads.
type PullRequestResult struct {
	PullRequest *PullRequest `json:"pull_request,omitempty"`
	ETag        string       `json:"etag,omitempty"`
	NotModified bool         `json:"not_modified"`
	RateLimit   RateLimit    `json:"rate_limit"`
}

// PullRequestDiffResult holds an exact unified-diff response for later
// canonicalization. It is not, by itself, a publishable revision identity.
type PullRequestDiffResult struct {
	Bytes       []byte    `json:"-"`
	SHA256      string    `json:"sha256,omitempty"`
	ETag        string    `json:"etag,omitempty"`
	NotModified bool      `json:"not_modified"`
	RateLimit   RateLimit `json:"rate_limit"`
}

// PullRequestFile is one provider-reported changed path. PatchPresent
// distinguishes an omitted patch from an intentionally empty text patch.
type PullRequestFile struct {
	Path         string `json:"path"`
	PreviousPath string `json:"previous_path,omitempty"`
	Status       string `json:"status"`
	SHA          string `json:"sha"`
	Patch        []byte `json:"-"`
	PatchPresent bool   `json:"patch_present"`
}

// PullRequestFilesPage is one complete page from GitHub's changed-file list.
type PullRequestFilesPage struct {
	Files        []PullRequestFile `json:"files"`
	NextPage     int               `json:"next_page,omitempty"`
	LimitReached bool              `json:"limit_reached"`
	RateLimit    RateLimit         `json:"rate_limit"`
}

// GitTreeEntry is a non-directory Git object from a recursive tree read.
type GitTreeEntry struct {
	Path       string `json:"path"`
	SHA        string `json:"sha"`
	Mode       string `json:"mode"`
	ObjectType string `json:"object_type"`
}

// GitTreeResult preserves GitHub's truncation signal. A truncated tree cannot
// prove complete canonical diff coverage.
type GitTreeResult struct {
	Entries   []GitTreeEntry `json:"entries"`
	Truncated bool           `json:"truncated"`
	RateLimit RateLimit      `json:"rate_limit"`
}
