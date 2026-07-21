// Package hydrate turns a selected metadata observation into an immutable
// canonical revision only after independently bounded GitHub evidence agrees.
package hydrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/application/canonical"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

const maxFilePages = 30

// Reader is the narrow read-only GitHub capability required to prove a
// canonical revision. Its methods are the relevant subset of the existing
// GitHub Reader, DiffReader, and HydrationReader capabilities.
type Reader interface {
	github.DiffReader
	github.HydrationReader
	GetPullRequest(context.Context, string, string, int, string) (github.PullRequestResult, error)
}

var _ Reader = (*github.Client)(nil)

// Store is the narrow persistence boundary used by hydration.
type Store interface {
	FindCanonicalHydrationTarget(context.Context, string, string, string, int) (sqlite.CanonicalHydrationTarget, error)
	AttachCanonicalRevision(context.Context, sqlite.CanonicalRevisionInput) (sqlite.CanonicalRevisionResult, error)
}

// Request identifies the selected current observation to hydrate.
type Request struct {
	ConnectionID string
	Owner        string
	Repository   string
	Number       int
}

// Result names the durable proof records created or reused by Hydrate.
type Result struct {
	Target     sqlite.CanonicalHydrationTarget
	Revision   canonical.Revision
	Attachment sqlite.CanonicalRevisionResult
}

// Service verifies complete GitHub evidence before attaching a canonical
// revision. It has no publish, job, event, or outbox capability.
type Service struct {
	Reader Reader
	Store  Store
	Now    func() time.Time
}

// Hydrate fetches bounded evidence for a selected observation and atomically
// attaches the resulting canonical manifest. Any changed or partial source
// fact fails closed before persistence is called.
func (s Service) Hydrate(ctx context.Context, request Request) (Result, error) {
	if s.Reader == nil || s.Store == nil {
		return Result{}, errors.New("canonical hydration requires reader and store")
	}
	if request.ConnectionID == "" || request.Owner == "" || request.Repository == "" || strings.Contains(request.Owner, "/") || strings.Contains(request.Repository, "/") || request.Number <= 0 {
		return Result{}, errors.New("canonical hydration request identity is required")
	}
	target, err := s.Store.FindCanonicalHydrationTarget(ctx, request.ConnectionID, request.Owner, request.Repository, request.Number)
	if err != nil {
		return Result{}, fmt.Errorf("find canonical hydration target: %w", err)
	}
	if target.ConnectionID != request.ConnectionID || !strings.EqualFold(target.Owner, request.Owner) || !strings.EqualFold(target.Repository, request.Repository) || target.Number != request.Number {
		return Result{}, errors.New("canonical hydration target does not match request")
	}

	if err := s.verifyPullRequest(ctx, target); err != nil {
		return Result{}, err
	}
	parsed, err := s.readDiff(ctx, target)
	if err != nil {
		return Result{}, err
	}
	files, err := s.readFiles(ctx, target, parsed)
	if err != nil {
		return Result{}, err
	}
	baseTree, err := s.readTree(ctx, target, target.BaseSHA)
	if err != nil {
		return Result{}, err
	}
	headTree, err := s.readTree(ctx, target, target.HeadSHA)
	if err != nil {
		return Result{}, err
	}
	changes, err := mapChanges(files, baseTree, headTree)
	if err != nil {
		return Result{}, err
	}
	revision, err := canonical.Build(canonical.Input{HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, Complete: true, Files: changes})
	if err != nil {
		return Result{}, fmt.Errorf("build canonical revision: %w", err)
	}
	if err := s.verifyPullRequest(ctx, target); err != nil {
		return Result{}, err
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	attachment, err := s.Store.AttachCanonicalRevision(ctx, sqlite.CanonicalRevisionInput{
		ConnectionID:   target.ConnectionID,
		ObservationID:  target.ObservationID,
		HeadSHA:        target.HeadSHA,
		BaseSHA:        target.BaseSHA,
		IdentityKey:    revision.IdentityKey,
		ManifestSHA256: revision.ManifestSHA256,
		ManifestJSON:   revision.Manifest,
		EntryCount:     revision.EntryCount,
		AttachedAt:     now,
	})
	if err != nil {
		return Result{}, fmt.Errorf("attach canonical revision: %w", err)
	}
	return Result{Target: target, Revision: revision, Attachment: attachment}, nil
}

func (s Service) verifyPullRequest(ctx context.Context, target sqlite.CanonicalHydrationTarget) error {
	result, err := s.Reader.GetPullRequest(ctx, target.Owner, target.Repository, target.Number, "")
	if err != nil {
		return fmt.Errorf("read pull request revision: %w", err)
	}
	if result.NotModified || result.PullRequest == nil {
		return errors.New("canonical hydration requires an authoritative pull request response")
	}
	if result.PullRequest.HeadSHA != target.HeadSHA || result.PullRequest.BaseSHA != target.BaseSHA {
		return errors.New("pull request head or base changed during canonical hydration")
	}
	return nil
}

func (s Service) readDiff(ctx context.Context, target sqlite.CanonicalHydrationTarget) (github.ParsedUnifiedDiff, error) {
	result, err := s.Reader.GetPullRequestDiff(ctx, target.Owner, target.Repository, target.Number, "")
	if err != nil {
		return github.ParsedUnifiedDiff{}, fmt.Errorf("read pull request diff: %w", err)
	}
	if result.NotModified {
		return github.ParsedUnifiedDiff{}, errors.New("canonical hydration requires an exact pull request diff response")
	}
	digest := sha256.Sum256(result.Bytes)
	if result.SHA256 != hex.EncodeToString(digest[:]) {
		return github.ParsedUnifiedDiff{}, errors.New("pull request diff digest does not match exact bytes")
	}
	parsed, err := github.ParseUnifiedDiff(result.Bytes)
	if err != nil {
		return github.ParsedUnifiedDiff{}, fmt.Errorf("parse pull request diff: %w", err)
	}
	return parsed, nil
}

func (s Service) readFiles(ctx context.Context, target sqlite.CanonicalHydrationTarget, parsed github.ParsedUnifiedDiff) ([]github.PullRequestFile, error) {
	files := make([]github.PullRequestFile, 0)
	seen := make(map[string]struct{})
	for page := 1; ; {
		result, err := s.Reader.GetPullRequestFiles(ctx, target.Owner, target.Repository, target.Number, page)
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

func (s Service) readTree(ctx context.Context, target sqlite.CanonicalHydrationTarget, commitSHA string) (map[string]github.GitTreeEntry, error) {
	result, err := s.Reader.GetGitTree(ctx, target.Owner, target.Repository, commitSHA)
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
