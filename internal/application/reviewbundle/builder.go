// Package reviewbundle creates a bounded, read-only engine input from current
// GitHub evidence and a selected canonical revision.
package reviewbundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/application/assessment"
	"github.com/sephriot/code-reviewer/internal/application/canonical"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

const (
	maxFilePages   = 30
	maxBundleBytes = 16 << 20
)

var hunkHeader = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)

// Reader is the narrow GET-only GitHub capability required to rebuild review
// evidence. It deliberately has no persistence or publication methods.
type Reader interface {
	github.DiffReader
	github.HydrationReader
	GetPullRequest(context.Context, string, string, int, string) (github.PullRequestResult, error)
}

var _ Reader = (*github.Client)(nil)

// Coordinate identifies the repository pull request fetched for a review.
type Coordinate struct {
	Owner      string
	Repository string
	Number     int
}

// VerifiedProfile is immutable profile content already verified by the caller
// against its selected profile version. Builder never reads or writes profile
// state itself.
type VerifiedProfile struct {
	ProfileID        string
	ProfileVersionID string
	Name             string
	Description      string
	Instructions     string
	SettingsJSON     []byte
}

// Input contains all immutable facts needed to build one engine request.
type Input struct {
	Target     sqlite.CanonicalReviewTarget
	Profile    VerifiedProfile
	Coordinate Coordinate
}

// Result contains one bounded engine JSON document and anchor evidence for
// validating the engine's assessment output.
type Result struct {
	Bundle   json.RawMessage
	Evidence assessment.RevisionEvidence
	Revision canonical.Revision
}

// Build independently re-reads bounded GitHub facts, rebuilds the exact
// canonical revision, and rejects any drift before returning an engine bundle.
func Build(ctx context.Context, reader Reader, input Input) (Result, error) {
	if reader == nil {
		return Result{}, errors.New("review bundle reader is required")
	}
	if err := validateInput(input); err != nil {
		return Result{}, err
	}
	if err := verifyPullRequest(ctx, reader, input.Coordinate, input.Target); err != nil {
		return Result{}, err
	}
	parsed, rawDiff, err := readDiff(ctx, reader, input.Coordinate)
	if err != nil {
		return Result{}, err
	}
	files, err := readFiles(ctx, reader, input.Coordinate, parsed)
	if err != nil {
		return Result{}, err
	}
	baseTree, err := readTree(ctx, reader, input.Coordinate, input.Target.BaseSHA)
	if err != nil {
		return Result{}, err
	}
	headTree, err := readTree(ctx, reader, input.Coordinate, input.Target.HeadSHA)
	if err != nil {
		return Result{}, err
	}
	changes, err := mapChanges(files, baseTree, headTree)
	if err != nil {
		return Result{}, err
	}
	revision, err := canonical.Build(canonical.Input{
		HeadSHA: input.Target.HeadSHA, BaseSHA: input.Target.BaseSHA, Complete: true, Files: changes,
	})
	if err != nil {
		return Result{}, fmt.Errorf("build canonical revision: %w", err)
	}
	if !sameCanonicalRevision(revision, input.Target) {
		return Result{}, errors.New("rebuilt canonical revision does not match selected evidence")
	}
	evidence, err := buildEvidence(input.Target, parsed)
	if err != nil {
		return Result{}, err
	}
	if err := verifyPullRequest(ctx, reader, input.Coordinate, input.Target); err != nil {
		return Result{}, err
	}
	bundle, err := marshalBundle(input, revision, rawDiff)
	if err != nil {
		return Result{}, err
	}
	return Result{Bundle: bundle, Evidence: evidence, Revision: revision}, nil
}

func validateInput(input Input) error {
	target := input.Target
	if strings.TrimSpace(target.ConnectionID) == "" || strings.TrimSpace(target.PullRequestID) == "" ||
		strings.TrimSpace(target.RepositoryID) == "" || strings.TrimSpace(target.ObservationID) == "" ||
		strings.TrimSpace(target.RevisionID) == "" || strings.TrimSpace(target.ManifestID) == "" {
		return errors.New("review bundle canonical target identity is required")
	}
	verified, err := canonical.Validate(target.ManifestJSON)
	if err != nil || !sameCanonicalRevision(verified, target) || verified.EntryCount != target.EntryCount ||
		verified.ManifestSHA256 != target.ManifestSHA256 || verified.IdentityKey != target.IdentityKey ||
		!bytes.Equal(verified.Manifest, target.ManifestJSON) ||
		verified.IdentityKey != "canonical_diff:v1:"+target.HeadSHA+":"+target.BaseSHA+":"+target.ManifestSHA256 {
		return errors.New("review bundle canonical target evidence is invalid")
	}
	if input.Coordinate.Owner == "" || input.Coordinate.Repository == "" ||
		strings.Contains(input.Coordinate.Owner, "/") || strings.Contains(input.Coordinate.Repository, "/") || input.Coordinate.Number <= 0 {
		return errors.New("review bundle repository coordinate is invalid")
	}
	profile := input.Profile
	if strings.TrimSpace(profile.ProfileID) == "" || strings.TrimSpace(profile.ProfileVersionID) == "" ||
		strings.TrimSpace(profile.Name) == "" || strings.TrimSpace(profile.Instructions) == "" {
		return errors.New("review bundle verified profile is invalid")
	}
	if _, err := normalizeJSONObject(profile.SettingsJSON); err != nil {
		return fmt.Errorf("review bundle profile settings: %w", err)
	}
	return nil
}

func verifyPullRequest(ctx context.Context, reader Reader, coordinate Coordinate, target sqlite.CanonicalReviewTarget) error {
	result, err := reader.GetPullRequest(ctx, coordinate.Owner, coordinate.Repository, coordinate.Number, "")
	if err != nil {
		return fmt.Errorf("read pull request revision: %w", err)
	}
	if result.NotModified || result.PullRequest == nil {
		return errors.New("review bundle requires an authoritative pull request response")
	}
	if result.PullRequest.HeadSHA != target.HeadSHA || result.PullRequest.BaseSHA != target.BaseSHA {
		return errors.New("pull request head or base changed during review bundle build")
	}
	return nil
}

func readDiff(ctx context.Context, reader Reader, coordinate Coordinate) (github.ParsedUnifiedDiff, []byte, error) {
	result, err := reader.GetPullRequestDiff(ctx, coordinate.Owner, coordinate.Repository, coordinate.Number, "")
	if err != nil {
		return github.ParsedUnifiedDiff{}, nil, fmt.Errorf("read pull request diff: %w", err)
	}
	if result.NotModified {
		return github.ParsedUnifiedDiff{}, nil, errors.New("review bundle requires an exact pull request diff response")
	}
	digest := sha256.Sum256(result.Bytes)
	if result.SHA256 != hex.EncodeToString(digest[:]) {
		return github.ParsedUnifiedDiff{}, nil, errors.New("pull request diff digest does not match exact bytes")
	}
	parsed, err := github.ParseUnifiedDiff(result.Bytes)
	if err != nil {
		return github.ParsedUnifiedDiff{}, nil, fmt.Errorf("parse pull request diff: %w", err)
	}
	return parsed, append([]byte(nil), result.Bytes...), nil
}

func readFiles(ctx context.Context, reader Reader, coordinate Coordinate, parsed github.ParsedUnifiedDiff) ([]github.PullRequestFile, error) {
	files := make([]github.PullRequestFile, 0, len(parsed.Files))
	seen := make(map[string]struct{}, len(parsed.Files))
	for page := 1; ; {
		result, err := reader.GetPullRequestFiles(ctx, coordinate.Owner, coordinate.Repository, coordinate.Number, page)
		if err != nil {
			return nil, fmt.Errorf("read pull request file page %d: %w", page, err)
		}
		if result.LimitReached {
			return nil, errors.New("pull request file coverage reached provider limit")
		}
		if len(result.Files) == 0 && result.NextPage != 0 {
			return nil, errors.New("pull request file pagination has empty non-final page")
		}
		for _, file := range result.Files {
			if _, duplicate := seen[file.Path]; duplicate {
				return nil, fmt.Errorf("pull request file coverage contains duplicate path %q", file.Path)
			}
			seen[file.Path] = struct{}{}
			proof, found := parsed.Files[file.Path]
			if !found || proof.PreviousPath != file.PreviousPath || proof.Binary {
				return nil, fmt.Errorf("pull request file %q lacks verified text patch", file.Path)
			}
			file.Patch, file.PatchPresent = proof.Fragment, true
			files = append(files, file)
		}
		if result.NextPage == 0 {
			if len(files) != len(parsed.Files) {
				return nil, errors.New("pull request diff and file coverage disagree")
			}
			return files, nil
		}
		if result.NextPage != page+1 || result.NextPage > maxFilePages {
			return nil, errors.New("pull request file pagination is not a bounded sequence")
		}
		page = result.NextPage
	}
}

func readTree(ctx context.Context, reader Reader, coordinate Coordinate, commitSHA string) (map[string]github.GitTreeEntry, error) {
	result, err := reader.GetGitTree(ctx, coordinate.Owner, coordinate.Repository, commitSHA)
	if err != nil {
		return nil, fmt.Errorf("read git tree %s: %w", commitSHA, err)
	}
	if result.Truncated {
		return nil, errors.New("git tree is truncated and cannot prove canonical coverage")
	}
	entries := make(map[string]github.GitTreeEntry, len(result.Entries))
	for _, entry := range result.Entries {
		if entry.ObjectType != "blob" {
			return nil, fmt.Errorf("git tree contains unsupported object at %q", entry.Path)
		}
		if _, duplicate := entries[entry.Path]; duplicate {
			return nil, fmt.Errorf("git tree contains duplicate path %q", entry.Path)
		}
		entries[entry.Path] = entry
	}
	return entries, nil
}

func mapChanges(files []github.PullRequestFile, baseTree, headTree map[string]github.GitTreeEntry) ([]canonical.FileChange, error) {
	changes := make([]canonical.FileChange, 0, len(files))
	for _, file := range files {
		change := canonical.FileChange{Path: file.Path, PreviousPath: file.PreviousPath, Status: file.Status, Patch: file.Patch, PatchPresent: file.PatchPresent}
		if file.Status != "added" {
			basePath := file.Path
			if file.Status == "renamed" {
				basePath = file.PreviousPath
			}
			entry, found := baseTree[basePath]
			if !found {
				return nil, fmt.Errorf("base tree lacks changed path %q", basePath)
			}
			change.BaseBlobSHA, change.BaseMode = entry.SHA, entry.Mode
		}
		if file.Status != "removed" {
			entry, found := headTree[file.Path]
			if !found {
				return nil, fmt.Errorf("head tree lacks changed path %q", file.Path)
			}
			if entry.SHA != file.SHA {
				return nil, fmt.Errorf("head tree blob does not match pull request file %q", file.Path)
			}
			change.HeadBlobSHA, change.HeadMode = entry.SHA, entry.Mode
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func buildEvidence(target sqlite.CanonicalReviewTarget, parsed github.ParsedUnifiedDiff) (assessment.RevisionEvidence, error) {
	paths := make([]string, 0, len(parsed.Files))
	for path := range parsed.Files {
		paths = append(paths, path)
	}
	sortStrings(paths)
	evidence := assessment.RevisionEvidence{HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, Files: make([]assessment.FileEvidence, 0, len(paths))}
	for _, path := range paths {
		proof := parsed.Files[path]
		left, right, err := parseHunkRanges(proof.Fragment)
		if err != nil {
			return assessment.RevisionEvidence{}, fmt.Errorf("parse pull request diff ranges for %q: %w", path, err)
		}
		evidence.Files = append(evidence.Files, assessment.FileEvidence{Path: path, Left: left, Right: right})
	}
	return evidence, nil
}

func parseHunkRanges(fragment []byte) ([]assessment.LineRange, []assessment.LineRange, error) {
	var left, right []assessment.LineRange
	lines := bytes.Split(fragment, []byte("\n"))
	for index := 0; index < len(lines); index++ {
		match := hunkHeader.FindSubmatch(lines[index])
		if match == nil {
			continue
		}
		oldStart, oldCount, err := parseHunkPosition(match[1], match[2])
		if err != nil {
			return nil, nil, err
		}
		newStart, newCount, err := parseHunkPosition(match[3], match[4])
		if err != nil {
			return nil, nil, err
		}
		oldLine, newLine := oldStart, newStart
		oldSeen, newSeen := 0, 0
		for index++; index < len(lines); index++ {
			line := lines[index]
			if hunkHeader.Match(line) || bytes.HasPrefix(line, []byte("diff --git ")) {
				index--
				break
			}
			if bytes.Equal(line, []byte(`\ No newline at end of file`)) {
				continue
			}
			if len(line) == 0 && index == len(lines)-1 {
				break
			}
			if len(line) == 0 {
				return nil, nil, errors.New("empty line inside unified diff hunk")
			}
			switch line[0] {
			case ' ':
				appendLine(&left, oldLine)
				appendLine(&right, newLine)
				oldLine, newLine, oldSeen, newSeen = oldLine+1, newLine+1, oldSeen+1, newSeen+1
			case '-':
				appendLine(&left, oldLine)
				oldLine, oldSeen = oldLine+1, oldSeen+1
			case '+':
				appendLine(&right, newLine)
				newLine, newSeen = newLine+1, newSeen+1
			default:
				return nil, nil, errors.New("invalid line inside unified diff hunk")
			}
			if oldSeen > oldCount || newSeen > newCount {
				return nil, nil, errors.New("unified diff hunk exceeds declared range")
			}
			if oldSeen == oldCount && newSeen == newCount {
				break
			}
		}
		if oldSeen != oldCount || newSeen != newCount {
			return nil, nil, errors.New("unified diff hunk does not satisfy declared range")
		}
	}
	return left, right, nil
}

func parseHunkPosition(startRaw, countRaw []byte) (int, int, error) {
	start, err := strconv.Atoi(string(startRaw))
	if err != nil || start < 0 {
		return 0, 0, errors.New("unified diff hunk start is invalid")
	}
	count := 1
	if len(countRaw) != 0 {
		count, err = strconv.Atoi(string(countRaw))
		if err != nil || count < 0 {
			return 0, 0, errors.New("unified diff hunk count is invalid")
		}
	}
	if count > 0 && start == 0 {
		return 0, 0, errors.New("unified diff hunk start is invalid")
	}
	return start, count, nil
}

func appendLine(ranges *[]assessment.LineRange, line int) {
	if len(*ranges) != 0 && (*ranges)[len(*ranges)-1].End+1 == line {
		(*ranges)[len(*ranges)-1].End = line
		return
	}
	*ranges = append(*ranges, assessment.LineRange{Start: line, End: line})
}

func sameCanonicalRevision(revision canonical.Revision, target sqlite.CanonicalReviewTarget) bool {
	return revision.IdentityKey == target.IdentityKey && revision.ManifestSHA256 == target.ManifestSHA256 &&
		revision.EntryCount == target.EntryCount && bytes.Equal(revision.Manifest, target.ManifestJSON)
}

func marshalBundle(input Input, revision canonical.Revision, rawDiff []byte) (json.RawMessage, error) {
	settings, err := normalizeJSONObject(input.Profile.SettingsJSON)
	if err != nil {
		return nil, fmt.Errorf("normalize review profile settings: %w", err)
	}
	type profile struct {
		ID           string          `json:"id"`
		VersionID    string          `json:"version_id"`
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		Instructions string          `json:"instructions"`
		Settings     json.RawMessage `json:"settings"`
	}
	type bundle struct {
		Version int `json:"version"`
		Review  struct {
			ConnectionID  string `json:"connection_id"`
			PullRequestID string `json:"pull_request_id"`
			RepositoryID  string `json:"repository_id"`
			ObservationID string `json:"observation_id"`
			RevisionID    string `json:"revision_id"`
			Owner         string `json:"owner"`
			Repository    string `json:"repository"`
			Number        int    `json:"number"`
		} `json:"review"`
		Profile   profile `json:"profile"`
		Canonical struct {
			IdentityKey    string          `json:"identity_key"`
			ManifestSHA256 string          `json:"manifest_sha256"`
			Manifest       json.RawMessage `json:"manifest"`
		} `json:"canonical"`
		Diff string `json:"unified_diff"`
	}
	value := bundle{Version: 1, Diff: string(rawDiff)}
	value.Review.ConnectionID, value.Review.PullRequestID, value.Review.RepositoryID = input.Target.ConnectionID, input.Target.PullRequestID, input.Target.RepositoryID
	value.Review.ObservationID, value.Review.RevisionID = input.Target.ObservationID, input.Target.RevisionID
	value.Review.Owner, value.Review.Repository, value.Review.Number = input.Coordinate.Owner, input.Coordinate.Repository, input.Coordinate.Number
	value.Profile = profile{ID: input.Profile.ProfileID, VersionID: input.Profile.ProfileVersionID, Name: input.Profile.Name, Description: input.Profile.Description, Instructions: input.Profile.Instructions, Settings: settings}
	value.Canonical.IdentityKey, value.Canonical.ManifestSHA256 = revision.IdentityKey, revision.ManifestSHA256
	value.Canonical.Manifest = append(json.RawMessage(nil), revision.Manifest...)
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode review engine bundle: %w", err)
	}
	if len(encoded) > maxBundleBytes {
		return nil, errors.New("review engine bundle exceeds 16 MiB")
	}
	return encoded, nil
}

func normalizeJSONObject(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, errors.New("must be a JSON object")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("must contain one JSON object")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode JSON object: %w", err)
	}
	return encoded, nil
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && values[cursor] < values[cursor-1]; cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}
