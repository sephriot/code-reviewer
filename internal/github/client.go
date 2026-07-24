package github

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/google/go-github/v68/github"
	"golang.org/x/oauth2"
)

type PRSummary struct {
	Owner     string
	Repo      string
	Number    int
	Title     string
	Author    string
	CommitSHA string
	Draft     bool
	State     string
}

type ReviewSubmission struct {
	Outcome  string
	Body     string
	Comments []ReviewComment
}

type ReviewComment struct {
	File    string
	Line    int
	Message string
}

type Client struct {
	*github.Client
	username string
}

func New(token, username string) *Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(context.Background(), ts)
	return &Client{
		Client:   github.NewClient(tc),
		username: username,
	}
}

func (c *Client) ListAssignedPRs(ctx context.Context) ([]PRSummary, error) {
	opts := &github.SearchOptions{
		Sort:  "updated",
		Order: "desc",
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}
	query := fmt.Sprintf("is:open is:pr review-requested:%s", c.username)
	result, _, err := c.Search.Issues(ctx, query, opts)
	if err != nil {
		return nil, fmt.Errorf("search assigned PRs: %w", err)
	}
	return prIssuesToSummaries(result.Issues), nil
}

func (c *Client) ListOwnPRs(ctx context.Context) ([]PRSummary, error) {
	opts := &github.SearchOptions{
		Sort:  "updated",
		Order: "desc",
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}
	query := fmt.Sprintf("is:open is:pr author:%s", c.username)
	result, _, err := c.Search.Issues(ctx, query, opts)
	if err != nil {
		return nil, fmt.Errorf("search own PRs: %w", err)
	}
	return prIssuesToSummaries(result.Issues), nil
}

func prIssuesToSummaries(issues []*github.Issue) []PRSummary {
	var summaries []PRSummary
	for _, issue := range issues {
		if issue.PullRequestLinks == nil {
			continue
		}
		repoFull := issue.GetRepositoryURL()
		owner, repo := parseRepoURL(repoFull)
		summaries = append(summaries, PRSummary{
			Owner:     owner,
			Repo:      repo,
			Number:    issue.GetNumber(),
			Title:     issue.GetTitle(),
			Author:    issue.GetUser().GetLogin(),
			CommitSHA: "",
			Draft:     false,
			State:     "open",
		})
	}
	return summaries
}

// NormalizePRState maps GitHub PR fields to our stored state.
// GitHub only returns open|closed; merged PRs are closed with merged=true.
func NormalizePRState(ghState string, merged bool) string {
	if merged {
		return "merged"
	}
	if ghState == "" {
		return "open"
	}
	return ghState
}

func (c *Client) GetPRDetails(ctx context.Context, owner, repo string, number int) (*PRSummary, error) {
	pr, _, err := c.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get PR %s/%s#%d: %w", owner, repo, number, err)
	}
	sha := ""
	if pr.Head != nil {
		sha = pr.Head.GetSHA()
	}
	return &PRSummary{
		Owner:     owner,
		Repo:      repo,
		Number:    pr.GetNumber(),
		Title:     pr.GetTitle(),
		Author:    pr.GetUser().GetLogin(),
		CommitSHA: sha,
		Draft:     pr.GetDraft(),
		State:     NormalizePRState(pr.GetState(), pr.GetMerged()),
	}, nil
}

func (c *Client) HasUserReviewed(ctx context.Context, owner, repo string, number int) (bool, error) {
	opts := &github.ListOptions{PerPage: 50}
	reviews, _, err := c.PullRequests.ListReviews(ctx, owner, repo, number, opts)
	if err != nil {
		return false, fmt.Errorf("list reviews for %s/%s#%d: %w", owner, repo, number, err)
	}
	for _, r := range reviews {
		if r.GetUser().GetLogin() == c.username {
			return true, nil
		}
	}
	return false, nil
}

func (c *Client) SubmitReview(ctx context.Context, owner, repo string, number int, submission ReviewSubmission) error {
	var event string
	switch submission.Outcome {
	case "approve_without_comments", "approve_with_comments":
		event = "APPROVE"
	case "changes_requested":
		event = "REQUEST_CHANGES"
	case "human_review":
		event = "COMMENT"
	case "tool_failed":
		event = "COMMENT"
	default:
		event = "COMMENT"
	}

	review := &github.PullRequestReviewRequest{
		Body:  &submission.Body,
		Event: &event,
	}
	for _, c := range submission.Comments {
		body := c.Message
		review.Comments = append(review.Comments, &github.DraftReviewComment{
			Path: &c.File,
			Body: &body,
			Line: &c.Line,
			Side: github.String("RIGHT"),
		})
	}

	_, _, err := c.PullRequests.CreateReview(ctx, owner, repo, number, review)
	if err != nil {
		return fmt.Errorf("submit review for %s/%s#%d: %w", owner, repo, number, err)
	}
	log.Printf("submitted review for %s/%s#%d (event=%s, comments=%d)", owner, repo, number, event, len(submission.Comments))
	return nil
}

func (c *Client) CreatePRComment(ctx context.Context, owner, repo string, number int, body string) error {
	comment := &github.IssueComment{
		Body: &body,
	}
	_, _, err := c.Issues.CreateComment(ctx, owner, repo, number, comment)
	if err != nil {
		return fmt.Errorf("create comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

func (c *Client) CreateReviewComment(ctx context.Context, owner, repo string, number int, comment ReviewComment) error {
	body := comment.Message
	_, _, err := c.PullRequests.CreateComment(ctx, owner, repo, number, &github.PullRequestComment{
		Body: &body,
		Path: &comment.File,
		Line: &comment.Line,
		Side: github.String("RIGHT"),
	})
	if err != nil {
		return fmt.Errorf("create review comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

func parseRepoURL(url string) (string, string) {
	parts := strings.Split(strings.TrimPrefix(url, "https://api.github.com/repos/"), "/")
	if len(parts) >= 2 {
		return parts[0], parts[1]
	}
	return "", ""
}

func (c *Client) GetFileContent(ctx context.Context, owner, repo, commitSHA, path string, line int, contextLines int) (content string, startLine int, err error) {
	opts := &github.RepositoryContentGetOptions{Ref: commitSHA}
	fileContent, _, _, err := c.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return "", 0, fmt.Errorf("get %s/%s/%s @ %s: %w", owner, repo, path, commitSHA, err)
	}
	decoded, err := fileContent.GetContent()
	if err != nil {
		return "", 0, fmt.Errorf("decode %s/%s/%s: %w", owner, repo, path, err)
	}
	lines := strings.Split(decoded, "\n")
	start := line - contextLines - 1
	if start < 0 {
		start = 0
	}
	end := line + contextLines
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[start:end], "\n"), start + 1, nil
}
