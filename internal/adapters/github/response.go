package github

import (
	"errors"
	"sort"
	"strings"
	"time"
)

type pullRequestResponse struct {
	ID      int64  `json:"id"`
	NodeID  string `json:"node_id"`
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	User    struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
		Login  string `json:"login"`
	} `json:"user"`
	State     string `json:"state"`
	Merged    bool   `json:"merged"`
	Draft     bool   `json:"draft"`
	UpdatedAt string `json:"updated_at"`
	Head      struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			ID       int64  `json:"id"`
			NodeID   string `json:"node_id"`
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	RequestedReviewers []struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
		Login  string `json:"login"`
	} `json:"requested_reviewers"`
}

func (response pullRequestResponse) normalize() (PullRequest, error) {
	headSHA, validHead := exactSHA(response.Head.SHA)
	baseSHA, validBase := exactSHA(response.Base.SHA)
	updatedAt, err := time.Parse(time.RFC3339, response.UpdatedAt)
	_, _, validRepository := splitFullName(response.Base.Repo.FullName)
	if response.ID <= 0 || response.NodeID == "" || response.Number <= 0 || response.User.ID <= 0 || response.User.Login == "" ||
		response.Base.Repo.ID <= 0 || response.Base.Repo.NodeID == "" || !validRepository || !validHead || !validBase || err != nil ||
		(response.State != "open" && response.State != "closed") || (response.Merged && response.State != "closed") {
		return PullRequest{}, errors.New("GitHub pull request response lacks canonical identity")
	}
	labels := make([]string, 0, len(response.Labels))
	for _, label := range response.Labels {
		if label.Name != "" {
			labels = append(labels, label.Name)
		}
	}
	sort.Strings(labels)
	reviewers := make([]User, 0, len(response.RequestedReviewers))
	for _, reviewer := range response.RequestedReviewers {
		if reviewer.ID <= 0 || reviewer.Login == "" {
			return PullRequest{}, errors.New("GitHub pull request contains invalid requested reviewer")
		}
		reviewers = append(reviewers, User{ID: reviewer.ID, NodeID: reviewer.NodeID, Login: reviewer.Login})
	}
	sort.Slice(reviewers, func(i, j int) bool { return reviewers[i].ID < reviewers[j].ID })
	return PullRequest{
		ID: response.ID, NodeID: response.NodeID, Number: response.Number,
		URL: response.HTMLURL, Title: response.Title, Body: response.Body,
		Author:           User{ID: response.User.ID, NodeID: response.User.NodeID, Login: response.User.Login},
		TargetRepository: Repository{ID: response.Base.Repo.ID, NodeID: response.Base.Repo.NodeID, FullName: response.Base.Repo.FullName},
		State:            response.State, Merged: response.Merged, Draft: response.Draft,
		HeadSHA: headSHA, BaseSHA: baseSHA, BaseRef: response.Base.Ref,
		Labels: labels, RequestedReviewers: reviewers, UpdatedAt: updatedAt.UTC(),
	}, nil
}

func splitFullName(fullName string) (string, string, bool) {
	owner, repository, ok := strings.Cut(fullName, "/")
	return owner, repository, ok && owner != "" && repository != "" && !strings.Contains(repository, "/")
}
