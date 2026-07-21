package canonical

import (
	"strings"
	"testing"
)

func TestBuildIsOrderAndLineEndingStable(t *testing.T) {
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	left, err := Build(Input{HeadSHA: head, BaseSHA: base, Complete: true, Files: []FileChange{
		{Path: "z.txt", Status: "modified", BaseBlobSHA: base, HeadBlobSHA: head, BaseMode: "100644", HeadMode: "100644", Patch: []byte("-old\r\n+new\r\n")},
		{Path: "a.txt", Status: "added", HeadBlobSHA: head, HeadMode: "100644", Patch: []byte("+new\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := Build(Input{HeadSHA: head, BaseSHA: base, Complete: true, Files: []FileChange{
		{Path: "a.txt", Status: "added", HeadBlobSHA: head, HeadMode: "100644", Patch: []byte("+new\n")},
		{Path: "z.txt", Status: "modified", BaseBlobSHA: base, HeadBlobSHA: head, BaseMode: "100644", HeadMode: "100644", Patch: []byte("-old\n+new\n")},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if left.IdentityKey != right.IdentityKey || left.ManifestSHA256 != right.ManifestSHA256 || string(left.Manifest) != string(right.Manifest) {
		t.Fatalf("canonical revisions differ: %+v / %+v", left, right)
	}
}

func TestBuildRejectsIncompleteOrAmbiguousCoverage(t *testing.T) {
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	tests := []Input{
		{HeadSHA: head, BaseSHA: base},
		{HeadSHA: head, BaseSHA: base, Complete: true, Files: []FileChange{{Path: "x", Status: "modified", BaseBlobSHA: base, HeadBlobSHA: head}, {Path: "x", Status: "modified", BaseBlobSHA: base, HeadBlobSHA: head}}},
		{HeadSHA: head, BaseSHA: base, Complete: true, Files: []FileChange{{Path: "../x", Status: "added", HeadBlobSHA: head}}},
	}
	for _, input := range tests {
		if _, err := Build(input); err == nil {
			t.Fatalf("Build accepted %+v", input)
		}
	}
}

func TestValidateRejectsMalformedOrNonCanonicalManifest(t *testing.T) {
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	if _, err := Validate([]byte(`{"version":1,"head_sha":"` + head + `","base_sha":"` + base + `","files":[{}]}`)); err == nil {
		t.Fatal("Validate accepted malformed file entry")
	}
	revision, err := Build(Input{HeadSHA: head, BaseSHA: base, Complete: true, Files: []FileChange{{Path: "a", Status: "added", HeadBlobSHA: head, HeadMode: "100644", Patch: []byte("+a\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(append(revision.Manifest, ' ')); err == nil {
		t.Fatal("Validate accepted non-canonical serialization")
	}
}
