package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxReviewBodyBytes    = 64 << 10
	maxReviewComments     = 500
	maxReviewCommentBytes = 64 << 10
)

// ReviewEvent is the only GitHub pull-request review state this adapter can submit.
type ReviewEvent string

const (
	ReviewEventApprove        ReviewEvent = "APPROVE"
	ReviewEventComment        ReviewEvent = "COMMENT"
	ReviewEventRequestChanges ReviewEvent = "REQUEST_CHANGES"
)

// ReviewComment is one immutable, already-validated diff anchor.
type ReviewComment struct {
	Path string
	Line int
	Side string
	Body string
}

// ReviewSubmission contains one GitHub review request. It carries no token
// and accepts only an exact current pull-request coordinate.
type ReviewSubmission struct {
	Owner      string
	Repository string
	Number     int
	Event      ReviewEvent
	Body       string
	Comments   []ReviewComment
}

// SubmittedReview identifies GitHub's accepted review artifact.
type SubmittedReview struct {
	ID     int64
	NodeID string
	State  string
}

// Publisher owns GitHub write capability. It is deliberately separate from
// Client, whose method set is permanently read-only.
type Publisher struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
	userAgent  string
}

// NewPublisher creates a bounded GitHub review publisher. Plain HTTP is only
// permitted for loopback test servers.
func NewPublisher(apiBaseURL, token string, httpClient *http.Client) (*Publisher, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("GitHub token is required")
	}
	base, err := url.Parse(apiBaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && !(base.Scheme == "http" && loopbackHost(base.Hostname()))) || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("GitHub API base URL is invalid")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	client := *httpClient
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	return &Publisher{baseURL: base, token: token, httpClient: &client, userAgent: defaultUserAgent}, nil
}

// SubmitReview posts one validated pull-request review. Callers must verify
// current canonical evidence and anchors immediately before calling it.
func (p *Publisher) SubmitReview(ctx context.Context, submission ReviewSubmission) (SubmittedReview, error) {
	if p == nil || ctx == nil {
		return SubmittedReview{}, errors.New("GitHub publisher and context are required")
	}
	if err := validateReviewSubmission(submission); err != nil {
		return SubmittedReview{}, err
	}
	payload, err := json.Marshal(struct {
		Body     string          `json:"body"`
		Event    ReviewEvent     `json:"event"`
		Comments []ReviewComment `json:"comments,omitempty"`
	}{submission.Body, submission.Event, submission.Comments})
	if err != nil {
		return SubmittedReview{}, fmt.Errorf("encode GitHub review: %w", err)
	}
	endpoint := *p.baseURL
	endpoint.Path = strings.TrimRight(p.baseURL.Path, "/") + "/repos/" + url.PathEscape(submission.Owner) + "/" + url.PathEscape(submission.Repository) + "/pulls/" + strconv.Itoa(submission.Number) + "/reviews"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return SubmittedReview{}, fmt.Errorf("create GitHub publication request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	request.Header.Set("User-Agent", p.userAgent)
	response, err := p.httpClient.Do(request)
	if err != nil {
		return SubmittedReview{}, fmt.Errorf("publish GitHub review: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return SubmittedReview{}, fmt.Errorf("read GitHub publication response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return SubmittedReview{}, errors.New("GitHub publication response exceeds 4 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return SubmittedReview{}, githubHTTPError(response, body, p.token)
	}
	var decoded struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
		State  string `json:"state"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return SubmittedReview{}, fmt.Errorf("decode GitHub publication response: %w", err)
	}
	if decoded.ID <= 0 || strings.TrimSpace(decoded.State) == "" {
		return SubmittedReview{}, errors.New("GitHub publication response lacks review identity")
	}
	return SubmittedReview{ID: decoded.ID, NodeID: decoded.NodeID, State: decoded.State}, nil
}

func validateReviewSubmission(value ReviewSubmission) error {
	if !validCoordinates(value.Owner, value.Repository, value.Number) || !validReviewEvent(value.Event) || len(value.Body) > maxReviewBodyBytes || len(value.Comments) > maxReviewComments {
		return errors.New("GitHub review submission is invalid")
	}
	for _, comment := range value.Comments {
		if strings.TrimSpace(comment.Path) == "" || strings.Contains(comment.Path, "\\") || comment.Line < 1 || (comment.Side != "LEFT" && comment.Side != "RIGHT") || strings.TrimSpace(comment.Body) == "" || len(comment.Body) > maxReviewCommentBytes {
			return errors.New("GitHub review comment is invalid")
		}
	}
	return nil
}

func validReviewEvent(value ReviewEvent) bool {
	return value == ReviewEventApprove || value == ReviewEventComment || value == ReviewEventRequestChanges
}
