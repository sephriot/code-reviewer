package hydrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

const (
	testHead = "1111111111111111111111111111111111111111"
	testBase = "2222222222222222222222222222222222222222"
	testBlob = "3333333333333333333333333333333333333333"
	testOld  = "4444444444444444444444444444444444444444"
)

func TestHydrateBuildsAndAttachesCompleteCanonicalRevision(t *testing.T) {
	reader := happyReader()
	store := &fakeStore{target: testTarget()}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	service := Service{Reader: reader, Store: store, Now: func() time.Time { return now }}

	result, err := service.Hydrate(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision.EntryCount != 2 || result.Attachment.RevisionID != "revision-1" || len(store.inputs) != 1 {
		t.Fatalf("result = %+v, attachments = %d", result, len(store.inputs))
	}
	input := store.inputs[0]
	if input.AttachedAt != now || input.HeadSHA != testHead || input.BaseSHA != testBase || input.EntryCount != 2 {
		t.Fatalf("attachment input = %+v", input)
	}
	if reader.pullCalls != 2 || reader.diffCalls != 1 || len(reader.filePages) != 2 || len(reader.treeCalls) != 2 {
		t.Fatalf("read calls pull=%d diff=%d files=%v trees=%v", reader.pullCalls, reader.diffCalls, reader.filePages, reader.treeCalls)
	}
	if !strings.Contains(string(input.ManifestJSON), "\"previous_path\":\"old.txt\"") {
		t.Fatalf("manifest omitted rename evidence: %s", input.ManifestJSON)
	}
}

func TestHydrateFailsClosedBeforeAttach(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeReader, *fakeStore)
		want   string
	}{
		{name: "changed before read", mutate: func(reader *fakeReader, _ *fakeStore) { reader.pulls[0].PullRequest.HeadSHA = testBase }, want: "head or base changed"},
		{name: "changed after evidence", mutate: func(reader *fakeReader, _ *fakeStore) { reader.pulls[1].PullRequest.BaseSHA = testHead }, want: "head or base changed"},
		{name: "bad diff digest", mutate: func(reader *fakeReader, _ *fakeStore) { reader.diff.SHA256 = strings.Repeat("0", 64) }, want: "diff digest"},
		{name: "provider file limit", mutate: func(reader *fakeReader, _ *fakeStore) {
			page := reader.files[1]
			page.LimitReached = true
			reader.files[1] = page
		}, want: "provider limit"},
		{name: "pagination jump", mutate: func(reader *fakeReader, _ *fakeStore) {
			page := reader.files[1]
			page.NextPage = 3
			reader.files[1] = page
		}, want: "pagination"},
		{name: "missing patch", mutate: func(reader *fakeReader, _ *fakeStore) {
			page := reader.files[1]
			page.Files[0].Path = "absent.txt"
			reader.files[1] = page
		}, want: "lacks verified text patch"},
		{name: "truncated tree", mutate: func(reader *fakeReader, _ *fakeStore) { reader.trees[testBase] = github.GitTreeResult{Truncated: true} }, want: "tree is truncated"},
		{name: "missing side blob", mutate: func(reader *fakeReader, _ *fakeStore) { reader.trees[testHead] = github.GitTreeResult{} }, want: "head tree lacks"},
		{name: "file tree mismatch", mutate: func(reader *fakeReader, _ *fakeStore) {
			reader.trees[testHead] = github.GitTreeResult{Entries: []github.GitTreeEntry{{Path: "new.txt", SHA: testOld, Mode: "100644", ObjectType: "blob"}, {Path: "renamed.txt", SHA: testBlob, Mode: "100644", ObjectType: "blob"}}}
		}, want: "does not match"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := happyReader()
			store := &fakeStore{target: testTarget()}
			test.mutate(reader, store)
			_, err := (Service{Reader: reader, Store: store}).Hydrate(context.Background(), testRequest())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if len(store.inputs) != 0 {
				t.Fatalf("attached despite failed evidence: %+v", store.inputs)
			}
		})
	}
}

func TestHydrateRejectsTargetMismatchAndAttachFailure(t *testing.T) {
	t.Run("target mismatch", func(t *testing.T) {
		store := &fakeStore{target: sqlite.CanonicalHydrationTarget{ConnectionID: "wrong", Owner: "owner", Repository: "repo", Number: 7}}
		_, err := (Service{Reader: happyReader(), Store: store}).Hydrate(context.Background(), testRequest())
		if err == nil || !strings.Contains(err.Error(), "target does not match") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("attach failure", func(t *testing.T) {
		store := &fakeStore{target: testTarget(), attachErr: errors.New("stale")}
		_, err := (Service{Reader: happyReader(), Store: store}).Hydrate(context.Background(), testRequest())
		if err == nil || !strings.Contains(err.Error(), "attach canonical revision: stale") {
			t.Fatalf("error = %v", err)
		}
	})
}

type fakeStore struct {
	target    sqlite.CanonicalHydrationTarget
	targetErr error
	attachErr error
	inputs    []sqlite.CanonicalRevisionInput
}

func (s *fakeStore) FindCanonicalHydrationTarget(_ context.Context, _, _, _ string, _ int) (sqlite.CanonicalHydrationTarget, error) {
	return s.target, s.targetErr
}

func (s *fakeStore) AttachCanonicalRevision(_ context.Context, input sqlite.CanonicalRevisionInput) (sqlite.CanonicalRevisionResult, error) {
	s.inputs = append(s.inputs, input)
	if s.attachErr != nil {
		return sqlite.CanonicalRevisionResult{}, s.attachErr
	}
	return sqlite.CanonicalRevisionResult{RevisionID: "revision-1", Created: true}, nil
}

type fakeReader struct {
	pulls     []github.PullRequestResult
	diff      github.PullRequestDiffResult
	files     map[int]github.PullRequestFilesPage
	trees     map[string]github.GitTreeResult
	pullCalls int
	diffCalls int
	filePages []int
	treeCalls []string
}

func (r *fakeReader) GetPullRequest(_ context.Context, _, _ string, _ int, _ string) (github.PullRequestResult, error) {
	if r.pullCalls >= len(r.pulls) {
		return github.PullRequestResult{}, errors.New("unexpected pull request read")
	}
	result := r.pulls[r.pullCalls]
	r.pullCalls++
	return result, nil
}
func (r *fakeReader) GetPullRequestDiff(context.Context, string, string, int, string) (github.PullRequestDiffResult, error) {
	r.diffCalls++
	return r.diff, nil
}
func (r *fakeReader) GetPullRequestFiles(_ context.Context, _ string, _ string, _ int, page int) (github.PullRequestFilesPage, error) {
	r.filePages = append(r.filePages, page)
	return r.files[page], nil
}
func (r *fakeReader) GetGitTree(_ context.Context, _ string, _ string, sha string) (github.GitTreeResult, error) {
	r.treeCalls = append(r.treeCalls, sha)
	return r.trees[sha], nil
}

func happyReader() *fakeReader {
	diff := []byte("diff --git a/new.txt b/new.txt\n@@ -0,0 +1 @@\n+new\ndiff --git a/old.txt b/renamed.txt\nsimilarity index 100%\nrename from old.txt\nrename to renamed.txt\n")
	digest := sha256.Sum256(diff)
	pre := github.PullRequestResult{PullRequest: &github.PullRequest{HeadSHA: testHead, BaseSHA: testBase}}
	post := github.PullRequestResult{PullRequest: &github.PullRequest{HeadSHA: testHead, BaseSHA: testBase}}
	return &fakeReader{
		pulls: []github.PullRequestResult{pre, post},
		diff:  github.PullRequestDiffResult{Bytes: diff, SHA256: hex.EncodeToString(digest[:])},
		files: map[int]github.PullRequestFilesPage{
			1: {Files: []github.PullRequestFile{{Path: "new.txt", Status: "added", SHA: testBlob, Patch: []byte("+new\n"), PatchPresent: true}}, NextPage: 2},
			2: {Files: []github.PullRequestFile{{Path: "renamed.txt", PreviousPath: "old.txt", Status: "renamed", SHA: testBlob, Patch: []byte("rename\n"), PatchPresent: true}}},
		},
		trees: map[string]github.GitTreeResult{
			testBase: {Entries: []github.GitTreeEntry{{Path: "old.txt", SHA: testOld, Mode: "100644", ObjectType: "blob"}}},
			testHead: {Entries: []github.GitTreeEntry{{Path: "new.txt", SHA: testBlob, Mode: "100644", ObjectType: "blob"}, {Path: "renamed.txt", SHA: testBlob, Mode: "100644", ObjectType: "blob"}}},
		},
	}
}

func testTarget() sqlite.CanonicalHydrationTarget {
	return sqlite.CanonicalHydrationTarget{ConnectionID: "connection-1", ObservationID: "observation-1", Owner: "owner", Repository: "repo", Number: 7, HeadSHA: testHead, BaseSHA: testBase}
}

func testRequest() Request {
	return Request{ConnectionID: "connection-1", Owner: "owner", Repository: "repo", Number: 7}
}
