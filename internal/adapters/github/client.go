// Package github provides a deliberately read-only GitHub adapter.
package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	apiVersion              = "2022-11-28"
	defaultUserAgent        = "code-reviewer-v2"
	maxResponseBytes        = 4 << 20
	maxDiffBytes            = 32 << 20
	maxTreeBytes            = 8 << 20
	maxPullRequestFilePages = 30
)

// HTTPError is a non-success GitHub response without credential disclosure.
type HTTPError struct {
	StatusCode int
	Message    string
	RateLimit  RateLimit
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("GitHub API returned %d: %s", e.StatusCode, e.Message)
}

// Client exposes only GitHub read operations required by shadow reconciliation.
type Client struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
	userAgent  string
}

// Reader is the complete GitHub capability exposed to shadow reconciliation.
// Its method set intentionally contains no mutation operation.
type Reader interface {
	AuthenticatedUser(context.Context) (AuthenticatedUserResult, error)
	SearchPullRequests(context.Context, string, int) (SearchPage, error)
	GetPullRequest(context.Context, string, string, int, string) (PullRequestResult, error)
}

// DiffReader exposes bounded, read-only unified-diff retrieval separately
// from metadata reconciliation.
type DiffReader interface {
	GetPullRequestDiff(context.Context, string, string, int, string) (PullRequestDiffResult, error)
}

// HydrationReader exposes full changed-path and tree facts needed to prove a
// canonical revision. It has no write capability.
type HydrationReader interface {
	GetPullRequestFiles(context.Context, string, string, int, int) (PullRequestFilesPage, error)
	GetGitTree(context.Context, string, string, string) (GitTreeResult, error)
}

var _ Reader = (*Client)(nil)
var _ DiffReader = (*Client)(nil)
var _ HydrationReader = (*Client)(nil)

// NewClient constructs a GET-only client. Plain HTTP is accepted only for a
// loopback test server.
func NewClient(apiBaseURL, token string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("GitHub token is required")
	}
	base, err := url.Parse(apiBaseURL)
	if err != nil || base.Host == "" {
		return nil, errors.New("GitHub API base URL is invalid")
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && loopbackHost(base.Hostname())) {
		return nil, errors.New("GitHub API base URL must use HTTPS or loopback HTTP")
	}
	if base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("GitHub API base URL cannot contain credentials, query, or fragment")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	readClient := *httpClient
	readClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 || request.URL.Scheme != base.Scheme || !strings.EqualFold(request.URL.Host, base.Host) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &Client{baseURL: base, token: token, httpClient: &readClient, userAgent: defaultUserAgent}, nil
}

// AuthenticatedUser returns the authoritative account identity for the token.
func (c *Client) AuthenticatedUser(ctx context.Context) (AuthenticatedUserResult, error) {
	var response struct {
		ID     int64  `json:"id"`
		NodeID string `json:"node_id"`
		Login  string `json:"login"`
	}
	metadata, err := c.getJSON(ctx, "/user", nil, "", &response)
	if err != nil {
		return AuthenticatedUserResult{}, err
	}
	if response.ID <= 0 || response.Login == "" {
		return AuthenticatedUserResult{}, errors.New("GitHub user response lacks identity")
	}
	return AuthenticatedUserResult{
		User:      User{ID: response.ID, NodeID: response.NodeID, Login: response.Login},
		RateLimit: metadata.rateLimit,
	}, nil
}

// SearchPullRequests reads one page from GitHub's issue search API.
func (c *Client) SearchPullRequests(ctx context.Context, query string, page int) (SearchPage, error) {
	if strings.TrimSpace(query) == "" || page < 1 {
		return SearchPage{}, errors.New("search query and positive page are required")
	}
	parameters := url.Values{
		"q": {query}, "sort": {"updated"}, "order": {"desc"},
		"per_page": {"100"}, "page": {strconv.Itoa(page)},
	}
	var response struct {
		TotalCount        int  `json:"total_count"`
		IncompleteResults bool `json:"incomplete_results"`
		Items             []struct {
			Number        int       `json:"number"`
			RepositoryURL string    `json:"repository_url"`
			PullRequest   *struct{} `json:"pull_request"`
		} `json:"items"`
	}
	metadata, err := c.getJSON(ctx, "/search/issues", parameters, "", &response)
	if err != nil {
		return SearchPage{}, err
	}
	result := SearchPage{
		TotalCount: response.TotalCount, IncompleteResults: response.IncompleteResults,
		RateLimit: metadata.rateLimit,
	}
	for _, item := range response.Items {
		if item.PullRequest == nil {
			continue
		}
		owner, repository, err := repositoryCoordinates(item.RepositoryURL)
		if err != nil || item.Number <= 0 {
			return SearchPage{}, errors.New("GitHub search response contains malformed pull request identity")
		}
		result.Candidates = append(result.Candidates, SearchCandidate{Owner: owner, Repository: repository, Number: item.Number})
	}
	result.NextPage, err = nextPage(metadata.link, page)
	if err != nil {
		return SearchPage{}, err
	}
	return result, nil
}

// GetPullRequest fetches authoritative PR detail. The target repository is
// always read from base.repo because head.repo may be a fork.
func (c *Client) GetPullRequest(ctx context.Context, owner, repository string, number int, etag string) (PullRequestResult, error) {
	if owner == "" || repository == "" || strings.Contains(owner, "/") || strings.Contains(repository, "/") || number <= 0 {
		return PullRequestResult{}, errors.New("valid repository coordinates and PR number are required")
	}
	var response pullRequestResponse
	metadata, err := c.getJSON(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repository)+"/pulls/"+strconv.Itoa(number), nil, etag, &response)
	if err != nil {
		return PullRequestResult{}, err
	}
	if metadata.notModified {
		return PullRequestResult{ETag: metadata.etag, NotModified: true, RateLimit: metadata.rateLimit}, nil
	}
	normalized, err := response.normalize()
	if err != nil {
		return PullRequestResult{}, err
	}
	if normalized.Number != number || !strings.EqualFold(normalized.TargetRepository.FullName, owner+"/"+repository) {
		return PullRequestResult{}, errors.New("GitHub pull request response does not match requested identity")
	}
	return PullRequestResult{PullRequest: &normalized, ETag: metadata.etag, RateLimit: metadata.rateLimit}, nil
}

// GetPullRequestDiff returns exact bounded unified-diff bytes for one PR.
// Callers must combine it with complete file and tree coverage before using
// it as a canonical revision identity.
func (c *Client) GetPullRequestDiff(ctx context.Context, owner, repository string, number int, etag string) (PullRequestDiffResult, error) {
	if owner == "" || repository == "" || strings.Contains(owner, "/") || strings.Contains(repository, "/") || number <= 0 {
		return PullRequestDiffResult{}, errors.New("valid repository coordinates and PR number are required")
	}
	metadata, body, err := c.getBytes(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repository)+"/pulls/"+strconv.Itoa(number), etag)
	if err != nil {
		return PullRequestDiffResult{}, err
	}
	if metadata.notModified {
		return PullRequestDiffResult{ETag: metadata.etag, NotModified: true, RateLimit: metadata.rateLimit}, nil
	}
	digest := sha256.Sum256(body)
	return PullRequestDiffResult{Bytes: body, SHA256: hex.EncodeToString(digest[:]), ETag: metadata.etag, RateLimit: metadata.rateLimit}, nil
}

// GetPullRequestFiles reads one bounded page of GitHub's changed-file list.
// Callers must follow NextPage and fail closed at GitHub's 3,000-file limit.
func (c *Client) GetPullRequestFiles(ctx context.Context, owner, repository string, number, page int) (PullRequestFilesPage, error) {
	if !validCoordinates(owner, repository, number) || page < 1 || page > maxPullRequestFilePages {
		return PullRequestFilesPage{}, errors.New("valid repository coordinates, PR number, and page are required")
	}
	parameters := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
	var response []struct {
		Path         string  `json:"filename"`
		PreviousPath string  `json:"previous_filename"`
		Status       string  `json:"status"`
		SHA          string  `json:"sha"`
		Patch        *string `json:"patch"`
	}
	metadata, err := c.getJSON(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repository)+"/pulls/"+strconv.Itoa(number)+"/files", parameters, "", &response)
	if err != nil {
		return PullRequestFilesPage{}, err
	}
	if len(response) > 100 {
		return PullRequestFilesPage{}, errors.New("GitHub pull request files response exceeds page limit")
	}
	result := PullRequestFilesPage{RateLimit: metadata.rateLimit}
	paths := make(map[string]struct{}, len(response))
	for _, item := range response {
		file, err := normalizePullRequestFile(item.Path, item.PreviousPath, item.Status, item.SHA, item.Patch)
		if err != nil {
			return PullRequestFilesPage{}, err
		}
		if _, exists := paths[file.Path]; exists {
			return PullRequestFilesPage{}, errors.New("GitHub pull request files response contains duplicate path")
		}
		paths[file.Path] = struct{}{}
		result.Files = append(result.Files, file)
	}
	result.NextPage, err = nextPage(metadata.link, page)
	if err != nil {
		return PullRequestFilesPage{}, err
	}
	result.LimitReached = page == maxPullRequestFilePages && len(result.Files) == 100
	return result, nil
}

// GetGitTree reads every non-directory entry reachable from an exact commit
// SHA. Truncated remains explicit so callers cannot treat partial coverage as
// canonical proof.
func (c *Client) GetGitTree(ctx context.Context, owner, repository, commitSHA string) (GitTreeResult, error) {
	if owner == "" || repository == "" || strings.Contains(owner, "/") || strings.Contains(repository, "/") || !validSHA(commitSHA) {
		return GitTreeResult{}, errors.New("valid repository coordinates and commit SHA are required")
	}
	var response struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"tree"`
	}
	metadata, err := c.getJSONLimit(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repository)+"/git/trees/"+commitSHA, url.Values{"recursive": {"1"}}, "", &response, maxTreeBytes)
	if err != nil {
		return GitTreeResult{}, err
	}
	result := GitTreeResult{Truncated: response.Truncated, RateLimit: metadata.rateLimit}
	paths := make(map[string]struct{}, len(response.Tree))
	for _, item := range response.Tree {
		if item.Type == "tree" {
			continue
		}
		if (item.Type != "blob" && item.Type != "commit") || !validRepositoryPath(item.Path) || !validSHA(item.SHA) || !validMode(item.Mode) {
			return GitTreeResult{}, errors.New("GitHub tree response contains malformed entry")
		}
		if _, exists := paths[item.Path]; exists {
			return GitTreeResult{}, errors.New("GitHub tree response contains duplicate path")
		}
		paths[item.Path] = struct{}{}
		result.Entries = append(result.Entries, GitTreeEntry{Path: item.Path, SHA: item.SHA, Mode: item.Mode, ObjectType: item.Type})
	}
	return result, nil
}

func normalizePullRequestFile(path, previousPath, status, sha string, patch *string) (PullRequestFile, error) {
	if !validRepositoryPath(path) || (previousPath != "" && !validRepositoryPath(previousPath)) || !validSHA(sha) {
		return PullRequestFile{}, errors.New("GitHub pull request files response contains malformed file")
	}
	if status != "added" && status != "modified" && status != "removed" && status != "renamed" {
		return PullRequestFile{}, errors.New("GitHub pull request files response contains unsupported status")
	}
	if status == "renamed" && previousPath == "" {
		return PullRequestFile{}, errors.New("GitHub renamed file response lacks previous path")
	}
	file := PullRequestFile{Path: path, PreviousPath: previousPath, Status: status, SHA: sha}
	if patch != nil {
		file.PatchPresent = true
		file.Patch = []byte(*patch)
	}
	return file, nil
}

func validCoordinates(owner, repository string, number int) bool {
	return owner != "" && repository != "" && !strings.Contains(owner, "/") && !strings.Contains(repository, "/") && number > 0
}

func validRepositoryPath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && !strings.Contains(value, "\\") && !strings.Contains(value, "\x00") && !strings.Contains(value, "../") && value != ".."
}

func validSHA(value string) bool {
	if len(value) != 40 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validMode(value string) bool {
	if len(value) != 6 {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '7' {
			return false
		}
	}
	return true
}

type responseMetadata struct {
	etag        string
	link        string
	notModified bool
	rateLimit   RateLimit
}

func (c *Client) getJSON(ctx context.Context, path string, parameters url.Values, etag string, target any) (responseMetadata, error) {
	return c.getJSONLimit(ctx, path, parameters, etag, target, maxResponseBytes)
}

func (c *Client) getJSONLimit(ctx context.Context, path string, parameters url.Values, etag string, target any, maximum int64) (responseMetadata, error) {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	endpoint.RawQuery = parameters.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return responseMetadata{}, fmt.Errorf("create GitHub request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	request.Header.Set("User-Agent", c.userAgent)
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return responseMetadata{}, fmt.Errorf("read GitHub API: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	metadata := responseMetadata{
		etag: response.Header.Get("ETag"), link: response.Header.Get("Link"),
		rateLimit: responseRateLimit(response.Header),
	}
	if response.StatusCode == http.StatusNotModified {
		if etag == "" {
			return responseMetadata{}, errors.New("GitHub returned 304 without a conditional request")
		}
		if metadata.etag == "" {
			metadata.etag = etag
		}
		metadata.notModified = true
		return metadata, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return responseMetadata{}, fmt.Errorf("read GitHub response: %w", err)
	}
	if len(body) > int(maximum) {
		return responseMetadata{}, fmt.Errorf("GitHub response exceeds %d MiB", maximum>>20)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseMetadata{}, githubHTTPError(response, body, c.token)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return responseMetadata{}, fmt.Errorf("decode GitHub response: %w", err)
	}
	return metadata, nil
}

func (c *Client) getBytes(ctx context.Context, path, etag string) (responseMetadata, []byte, error) {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(c.baseURL.Path, "/") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return responseMetadata{}, nil, fmt.Errorf("create GitHub request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/vnd.github.diff")
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	request.Header.Set("User-Agent", c.userAgent)
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return responseMetadata{}, nil, fmt.Errorf("read GitHub API: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	metadata := responseMetadata{etag: response.Header.Get("ETag"), rateLimit: responseRateLimit(response.Header)}
	if response.StatusCode == http.StatusNotModified {
		if etag == "" {
			return responseMetadata{}, nil, errors.New("GitHub returned 304 without a conditional request")
		}
		if metadata.etag == "" {
			metadata.etag = etag
		}
		metadata.notModified = true
		return metadata, nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDiffBytes+1))
	if err != nil {
		return responseMetadata{}, nil, fmt.Errorf("read GitHub diff: %w", err)
	}
	if len(body) > maxDiffBytes {
		return responseMetadata{}, nil, errors.New("GitHub diff exceeds 32 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseMetadata{}, nil, githubHTTPError(response, body, c.token)
	}
	return metadata, body, nil
}

func githubHTTPError(response *http.Response, body []byte, token string) error {
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.Message == "" {
		payload.Message = http.StatusText(response.StatusCode)
	}
	result := &HTTPError{
		StatusCode: response.StatusCode,
		Message:    sanitizeProviderMessage(payload.Message, token),
		RateLimit:  responseRateLimit(response.Header),
	}
	return result
}

func responseRateLimit(header http.Header) RateLimit {
	result := RateLimit{
		Limit:     parseNonNegativeHeader(header.Get("X-RateLimit-Limit")),
		Remaining: parseNonNegativeHeader(header.Get("X-RateLimit-Remaining")),
		Used:      parseNonNegativeHeader(header.Get("X-RateLimit-Used")),
		Resource:  header.Get("X-RateLimit-Resource"),
	}
	if epoch, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil && epoch > 0 {
		result.Reset = time.Unix(epoch, 0).UTC()
	}
	if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil && seconds > 0 {
		result.RetryAfter = time.Duration(seconds) * time.Second
	}
	return result
}

func parseNonNegativeHeader(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func sanitizeProviderMessage(message, token string) string {
	if token != "" {
		message = strings.ReplaceAll(message, token, "[REDACTED]")
	}
	message = strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	runes := []rune(message)
	if len(runes) > 512 {
		message = string(runes[:512])
	}
	return message
}

func repositoryCoordinates(repositoryURL string) (string, string, error) {
	parsed, err := url.Parse(repositoryURL)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 3 || parts[len(parts)-3] != "repos" {
		return "", "", errors.New("invalid repository URL")
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}

func nextPage(link string, currentPage int) (int, error) {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start, end := strings.Index(part, "<"), strings.Index(part, ">")
		if start < 0 || end <= start {
			return 0, errors.New("GitHub pagination next link is malformed")
		}
		parsed, err := url.Parse(part[start+1 : end])
		if err != nil {
			return 0, errors.New("GitHub pagination next link is malformed")
		}
		page, err := strconv.Atoi(parsed.Query().Get("page"))
		if err != nil || page != currentPage+1 {
			return 0, errors.New("GitHub pagination next page is not contiguous")
		}
		return page, nil
	}
	return 0, nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func exactSHA(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 40 {
		return "", false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return "", false
		}
	}
	return value, true
}
