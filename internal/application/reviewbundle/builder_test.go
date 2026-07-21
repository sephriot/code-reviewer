package reviewbundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/adapters/github"
	"github.com/sephriot/code-reviewer/internal/application/canonical"
	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

const (
	bundleHead = "1111111111111111111111111111111111111111"
	bundleBase = "2222222222222222222222222222222222222222"
	bundleOld  = "3333333333333333333333333333333333333333"
	bundleNew  = "4444444444444444444444444444444444444444"
)

func TestBuildRebuildsVerifiedEvidenceAndBoundedEngineBundle(t *testing.T) {
	reader, input := bundleFixture(t)
	result, err := Build(context.Background(), reader, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision.ManifestSHA256 != input.Target.ManifestSHA256 || reader.pullCalls != 2 || reader.diffCalls != 1 || len(reader.treeCalls) != 2 {
		t.Fatalf("result=%+v calls pull=%d diff=%d tree=%v", result.Revision, reader.pullCalls, reader.diffCalls, reader.treeCalls)
	}
	if len(result.Evidence.Files) != 1 || len(result.Evidence.Files[0].Left) != 1 || len(result.Evidence.Files[0].Right) != 1 ||
		result.Evidence.Files[0].Left[0].Start != 1 || result.Evidence.Files[0].Left[0].End != 2 ||
		result.Evidence.Files[0].Right[0].Start != 1 || result.Evidence.Files[0].Right[0].End != 2 {
		t.Fatalf("evidence=%+v", result.Evidence)
	}
	var bundle struct {
		Version int `json:"version"`
		Profile struct {
			Instructions string          `json:"instructions"`
			Settings     json.RawMessage `json:"settings"`
		} `json:"profile"`
		Canonical struct {
			ManifestSHA256 string `json:"manifest_sha256"`
		} `json:"canonical"`
		Diff string `json:"unified_diff"`
	}
	if err := json.Unmarshal(result.Bundle, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Version != 1 || bundle.Profile.Instructions != "Find bugs." || string(bundle.Profile.Settings) != `{"model":"test"}` ||
		bundle.Canonical.ManifestSHA256 != input.Target.ManifestSHA256 || !strings.Contains(bundle.Diff, "@@ -1,2 +1,2 @@") {
		t.Fatalf("bundle=%s", result.Bundle)
	}
}

func TestBuildRejectsEvidenceDriftBeforeReturningBundle(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*bundleReader, *Input)
		want   string
	}{
		{name: "changed pull request", mutate: func(reader *bundleReader, _ *Input) { reader.pulls[0].PullRequest.HeadSHA = bundleBase }, want: "head or base changed"},
		{name: "changed after reads", mutate: func(reader *bundleReader, _ *Input) { reader.pulls[1].PullRequest.BaseSHA = bundleHead }, want: "head or base changed"},
		{name: "bad diff digest", mutate: func(reader *bundleReader, _ *Input) { reader.diff.SHA256 = strings.Repeat("0", 64) }, want: "diff digest"},
		{name: "changed canonical evidence", mutate: func(reader *bundleReader, _ *Input) {
			reader.trees[bundleBase] = github.GitTreeResult{Entries: []github.GitTreeEntry{{Path: "file.txt", SHA: bundleOld, Mode: "100755", ObjectType: "blob"}}}
		}, want: "does not match selected evidence"},
		{name: "invalid hunk range", mutate: func(reader *bundleReader, input *Input) {
			reader.diff = makeDiffResult(t, []byte("diff --git a/file.txt b/file.txt\n--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n"))
			input.Target = targetForDiff(t, reader.diff.Bytes)
		}, want: "does not satisfy declared range"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader, input := bundleFixture(t)
			test.mutate(reader, &input)
			_, err := Build(context.Background(), reader, input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want %q", err, test.want)
			}
		})
	}
}

func TestBuildRejectsMalformedSelectedCanonicalTargetWithoutGitHubReads(t *testing.T) {
	reader, input := bundleFixture(t)
	input.Target.ManifestJSON = []byte(`{"version":1}`)
	_, err := Build(context.Background(), reader, input)
	if err == nil || !strings.Contains(err.Error(), "target evidence is invalid") {
		t.Fatalf("error=%v", err)
	}
	if reader.pullCalls != 0 || reader.diffCalls != 0 {
		t.Fatalf("GitHub was read for malformed selected target")
	}
}

type bundleReader struct {
	pulls     []github.PullRequestResult
	diff      github.PullRequestDiffResult
	files     map[int]github.PullRequestFilesPage
	trees     map[string]github.GitTreeResult
	pullCalls int
	diffCalls int
	treeCalls []string
}

func (r *bundleReader) GetPullRequest(_ context.Context, _, _ string, _ int, _ string) (github.PullRequestResult, error) {
	if r.pullCalls >= len(r.pulls) {
		return github.PullRequestResult{}, errors.New("unexpected pull request read")
	}
	result := r.pulls[r.pullCalls]
	r.pullCalls++
	return result, nil
}

func (r *bundleReader) GetPullRequestDiff(context.Context, string, string, int, string) (github.PullRequestDiffResult, error) {
	r.diffCalls++
	return r.diff, nil
}

func (r *bundleReader) GetPullRequestFiles(_ context.Context, _ string, _ string, _ int, page int) (github.PullRequestFilesPage, error) {
	return r.files[page], nil
}

func (r *bundleReader) GetGitTree(_ context.Context, _ string, _ string, sha string) (github.GitTreeResult, error) {
	r.treeCalls = append(r.treeCalls, sha)
	return r.trees[sha], nil
}

func bundleFixture(t *testing.T) (*bundleReader, Input) {
	t.Helper()
	diff := []byte("diff --git a/file.txt b/file.txt\nindex 3333333..4444444 100644\n--- a/file.txt\n+++ b/file.txt\n@@ -1,2 +1,2 @@\n-old\n keep\n+new\n")
	reader := &bundleReader{
		pulls: []github.PullRequestResult{
			{PullRequest: &github.PullRequest{HeadSHA: bundleHead, BaseSHA: bundleBase}},
			{PullRequest: &github.PullRequest{HeadSHA: bundleHead, BaseSHA: bundleBase}},
		},
		diff:  makeDiffResult(t, diff),
		files: map[int]github.PullRequestFilesPage{1: {Files: []github.PullRequestFile{{Path: "file.txt", Status: "modified", SHA: bundleNew}}}},
		trees: map[string]github.GitTreeResult{
			bundleBase: {Entries: []github.GitTreeEntry{{Path: "file.txt", SHA: bundleOld, Mode: "100644", ObjectType: "blob"}}},
			bundleHead: {Entries: []github.GitTreeEntry{{Path: "file.txt", SHA: bundleNew, Mode: "100644", ObjectType: "blob"}}},
		},
	}
	return reader, Input{
		Target:     targetForDiff(t, diff),
		Coordinate: Coordinate{Owner: "owner", Repository: "repo", Number: 7},
		Profile: VerifiedProfile{
			ProfileID: "profile-1", ProfileVersionID: "profile-version-1", Name: "Default", Description: "", Instructions: "Find bugs.", SettingsJSON: []byte(` { "model" : "test" } `),
		},
	}
}

func makeDiffResult(t *testing.T, diff []byte) github.PullRequestDiffResult {
	t.Helper()
	digest := sha256.Sum256(diff)
	return github.PullRequestDiffResult{Bytes: diff, SHA256: hex.EncodeToString(digest[:])}
}

func targetForDiff(t *testing.T, diff []byte) sqlite.CanonicalReviewTarget {
	t.Helper()
	parsed, err := github.ParseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	proof, found := parsed.Files["file.txt"]
	if !found {
		t.Fatal("fixture diff lacks file")
	}
	revision, err := canonical.Build(canonical.Input{HeadSHA: bundleHead, BaseSHA: bundleBase, Complete: true, Files: []canonical.FileChange{{
		Path: "file.txt", Status: "modified", BaseBlobSHA: bundleOld, HeadBlobSHA: bundleNew, BaseMode: "100644", HeadMode: "100644", Patch: proof.Fragment, PatchPresent: true,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return sqlite.CanonicalReviewTarget{
		ConnectionID: "connection-1", PullRequestID: "pr-1", RepositoryID: "repo-1", ObservationID: "observation-1", RevisionID: "revision-1", ManifestID: "manifest-1",
		HeadSHA: bundleHead, BaseSHA: bundleBase, IdentityKey: revision.IdentityKey, ManifestSHA256: revision.ManifestSHA256, ManifestJSON: revision.Manifest, EntryCount: revision.EntryCount,
	}
}
