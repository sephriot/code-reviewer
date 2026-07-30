package github

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

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
	UpdatedAt time.Time
}

type ReviewSubmission struct {
	Outcome  string
	Body     string
	Comments []ReviewComment
}

type ReviewComment struct {
	File     string
	Line     int
	Message  string
	CommitID string
}

type AssignmentSnapshot struct {
	PRs      []PRSummary
	Complete bool
}

type ReviewState string

const (
	ReviewStateCommented        ReviewState = "commented"
	ReviewStateApproved         ReviewState = "approved"
	ReviewStateChangesRequested ReviewState = "changes_requested"
)

type EffectiveReview struct {
	ID    int64
	State ReviewState
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

func (c *Client) ListReviewAssignments(ctx context.Context) (AssignmentSnapshot, error) {
	snapshot := AssignmentSnapshot{Complete: true}
	var discoveryErrors []error

	direct, complete, err := c.searchPRSummariesDetailed(
		ctx,
		fmt.Sprintf("is:open is:pr review-requested:%s", c.username),
		"search direct review assignments",
	)
	snapshot.PRs = append(snapshot.PRs, direct...)
	if err != nil {
		discoveryErrors = append(discoveryErrors, err)
	}
	if err != nil || !complete {
		snapshot.Complete = false
	}

	teams, err := c.listUserTeams(ctx)
	if err != nil {
		snapshot.Complete = false
		discoveryErrors = append(discoveryErrors, err)
	}
	for _, team := range teams {
		teamPRs, complete, err := c.searchPRSummariesDetailed(
			ctx,
			fmt.Sprintf("is:open is:pr team-review-requested:%s/%s", team.Organization.GetLogin(), team.GetSlug()),
			fmt.Sprintf("search review assignments for %s/%s", team.Organization.GetLogin(), team.GetSlug()),
		)
		snapshot.PRs = append(snapshot.PRs, teamPRs...)
		if err != nil {
			discoveryErrors = append(discoveryErrors, err)
		}
		if err != nil || !complete {
			snapshot.Complete = false
		}
	}

	deduplicated := make(map[string]PRSummary, len(snapshot.PRs))
	for _, pr := range snapshot.PRs {
		key := strings.ToLower(fmt.Sprintf("%s/%s#%d", pr.Owner, pr.Repo, pr.Number))
		deduplicated[key] = pr
	}
	snapshot.PRs = snapshot.PRs[:0]
	for _, pr := range deduplicated {
		snapshot.PRs = append(snapshot.PRs, pr)
	}
	sort.Slice(snapshot.PRs, func(i, j int) bool {
		left := fmt.Sprintf("%s/%s#%d", snapshot.PRs[i].Owner, snapshot.PRs[i].Repo, snapshot.PRs[i].Number)
		right := fmt.Sprintf("%s/%s#%d", snapshot.PRs[j].Owner, snapshot.PRs[j].Repo, snapshot.PRs[j].Number)
		return left < right
	})
	return snapshot, errors.Join(discoveryErrors...)
}

func (c *Client) searchPRSummariesDetailed(ctx context.Context, query, errLabel string) ([]PRSummary, bool, error) {
	opts := &github.SearchOptions{
		Sort:  "updated",
		Order: "desc",
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}
	var all []PRSummary
	complete := true
	for {
		result, resp, err := c.Search.Issues(ctx, query, opts)
		if err != nil {
			return all, false, fmt.Errorf("%s: %w", errLabel, err)
		}
		complete = complete && !result.GetIncompleteResults()
		all = append(all, prIssuesToSummaries(result.Issues)...)
		if resp == nil || resp.NextPage == 0 {
			return all, complete, nil
		}
		opts.Page = resp.NextPage
	}
}

func (c *Client) listUserTeams(ctx context.Context) ([]*github.Team, error) {
	opts := &github.ListOptions{PerPage: 100}
	var all []*github.Team
	for {
		teams, resp, err := c.Teams.ListUserTeams(ctx, opts)
		if err != nil {
			return all, fmt.Errorf("list authenticated user teams: %w", err)
		}
		for _, team := range teams {
			if team.Organization == nil || team.Organization.GetLogin() == "" || team.GetSlug() == "" {
				return all, fmt.Errorf("list authenticated user teams: team is missing organization or slug")
			}
			all = append(all, team)
		}
		if resp == nil || resp.NextPage == 0 {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
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
			UpdatedAt: issue.GetUpdatedAt().Time,
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
		UpdatedAt: pr.GetUpdatedAt().Time,
	}, nil
}

func (c *Client) GetEffectiveReview(ctx context.Context, owner, repo string, number int) (*EffectiveReview, error) {
	opts := &github.ListOptions{PerPage: 100}
	var latest *github.PullRequestReview
	for {
		reviews, resp, err := c.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list reviews for %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, r := range reviews {
			if !strings.EqualFold(r.GetUser().GetLogin(), c.username) {
				continue
			}
			if latest == nil || reviewAfter(r, latest) {
				latest = r
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	if latest == nil {
		return nil, nil
	}
	var state ReviewState
	switch strings.ToUpper(latest.GetState()) {
	case "COMMENTED":
		state = ReviewStateCommented
	case "APPROVED":
		state = ReviewStateApproved
	case "CHANGES_REQUESTED":
		state = ReviewStateChangesRequested
	default:
		return nil, nil
	}
	return &EffectiveReview{ID: latest.GetID(), State: state}, nil
}

func reviewAfter(left, right *github.PullRequestReview) bool {
	leftTime := left.GetSubmittedAt().Time
	rightTime := right.GetSubmittedAt().Time
	if leftTime.IsZero() || rightTime.IsZero() {
		return left.GetID() > right.GetID()
	}
	if leftTime.Equal(rightTime) {
		return left.GetID() > right.GetID()
	}
	return leftTime.After(rightTime)
}

func (c *Client) SubmitReview(ctx context.Context, owner, repo string, number int, submission ReviewSubmission) (int64, error) {
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

	created, _, err := c.PullRequests.CreateReview(ctx, owner, repo, number, review)
	if err != nil {
		return 0, fmt.Errorf("submit review for %s/%s#%d: %w", owner, repo, number, err)
	}
	log.Printf("submitted review for %s/%s#%d (event=%s, comments=%d)", owner, repo, number, event, len(submission.Comments))
	return created.GetID(), nil
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
	if comment.CommitID == "" {
		return fmt.Errorf("create review comment on %s/%s#%d: commit_id is required", owner, repo, number)
	}
	body := comment.Message
	_, _, err := c.PullRequests.CreateComment(ctx, owner, repo, number, &github.PullRequestComment{
		Body:     &body,
		Path:     &comment.File,
		Line:     &comment.Line,
		Side:     github.String("RIGHT"),
		CommitID: &comment.CommitID,
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
