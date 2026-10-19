package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v68/github"
)

type TimingCandidate struct {
	Owner     string
	Repo      string
	Number    int
	Author    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WalkTimingPRs splits large search ranges to avoid GitHub's 1,000-result search limit.
func (c *Client) WalkTimingPRs(ctx context.Context, direction string, since, before time.Time, updatedSince *time.Time, visit func(TimingCandidate) error) error {
	qualifier := "reviewed-by:"
	if direction == "received" {
		qualifier = "author:"
	} else if direction != "given" {
		return fmt.Errorf("unknown timing direction %q", direction)
	}
	query := fmt.Sprintf("is:pr %s%s created:%s..%s", qualifier, c.username, since.UTC().Format(time.RFC3339), before.Add(-time.Second).UTC().Format(time.RFC3339))
	if direction == "given" {
		query += " -author:" + c.username
	}
	if updatedSince != nil {
		query += " updated:>=" + updatedSince.UTC().Format(time.RFC3339)
	}
	opts := &github.SearchOptions{Sort: "created", Order: "desc", ListOptions: github.ListOptions{PerPage: 100}}
	for {
		result, resp, err := c.Search.Issues(ctx, query, opts)
		if err != nil {
			return fmt.Errorf("search review timing: %w", err)
		}
		if result.GetTotal() > 1000 || result.GetIncompleteResults() {
			if opts.Page > 1 || before.Sub(since) < 2*time.Second {
				return fmt.Errorf("incomplete review timing search for %s to %s", since, before)
			}
			middle := since.Add(before.Sub(since) / 2).Truncate(time.Second)
			if err := c.WalkTimingPRs(ctx, direction, middle, before, updatedSince, visit); err != nil {
				return err
			}
			return c.WalkTimingPRs(ctx, direction, since, middle, updatedSince, visit)
		}
		for _, issue := range result.Issues {
			if issue.PullRequestLinks == nil {
				continue
			}
			owner, repo := parseRepoURL(issue.GetRepositoryURL())
			candidate := TimingCandidate{Owner: owner, Repo: repo, Number: issue.GetNumber(), Author: issue.GetUser().GetLogin(), CreatedAt: issue.GetCreatedAt().Time, UpdatedAt: issue.GetUpdatedAt().Time}
			if owner == "" || repo == "" || candidate.Author == "" || candidate.Number <= 0 || candidate.CreatedAt.IsZero() || candidate.UpdatedAt.IsZero() {
				return fmt.Errorf("incomplete PR metadata in review timing search")
			}
			if err := visit(candidate); err != nil {
				return err
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return nil
		}
		opts.Page = resp.NextPage
	}
}

func (c *Client) FirstReviewTime(ctx context.Context, candidate TimingCandidate, direction string) (*time.Time, error) {
	opts := &github.ListOptions{PerPage: 100}
	var first *time.Time
	for {
		reviews, resp, err := c.PullRequests.ListReviews(ctx, candidate.Owner, candidate.Repo, candidate.Number, opts)
		if err != nil {
			return nil, fmt.Errorf("review timing %s/%s#%d: %w", candidate.Owner, candidate.Repo, candidate.Number, err)
		}
		for _, review := range reviews {
			login := review.GetUser().GetLogin()
			submitted := review.GetSubmittedAt().Time
			switch strings.ToUpper(review.GetState()) {
			case "APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED":
			default:
				continue
			}
			if submitted.IsZero() || submitted.Before(candidate.CreatedAt) || login == "" || strings.EqualFold(login, candidate.Author) {
				continue
			}
			if direction == "given" {
				if !strings.EqualFold(login, c.username) {
					continue
				}
			} else if direction == "received" {
				if strings.EqualFold(review.GetUser().GetType(), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") {
					continue
				}
			} else {
				return nil, fmt.Errorf("unknown timing direction %q", direction)
			}
			if first == nil || submitted.Before(*first) {
				t := submitted
				first = &t
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return first, nil
		}
		opts.Page = resp.NextPage
	}
}
