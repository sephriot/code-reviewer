package github

import (
	"bytes"
	"errors"
	"testing"
)

func TestParseUnifiedDiffSplitsExactFragments(t *testing.T) {
	raw := []byte("diff --git a/alpha.txt b/alpha.txt\nindex 1..2 100644\n--- a/alpha.txt\n+++ b/alpha.txt\n@@ -1 +1 @@\n-old\n+new\ndiff --git a/bin.dat b/bin.dat\nindex 3..4 100644\nBinary files a/bin.dat and b/bin.dat differ\n")

	parsed, err := ParseUnifiedDiff(raw)
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	if len(parsed.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(parsed.Files))
	}
	text := parsed.Files["alpha.txt"]
	if text.Path != "alpha.txt" || text.PreviousPath != "" || text.Binary {
		t.Fatalf("text = %#v", text)
	}
	if !bytes.Equal(text.Fragment, raw[:bytes.Index(raw, []byte("diff --git a/bin.dat"))]) {
		t.Fatalf("text fragment was not exact: %q", text.Fragment)
	}
	binary := parsed.Files["bin.dat"]
	if !binary.Binary || !bytes.Contains(binary.Fragment, []byte("Binary files")) {
		t.Fatalf("binary = %#v", binary)
	}
}

func TestParseUnifiedDiffRecognizesRenameAndGitBinaryPatch(t *testing.T) {
	raw := []byte("diff --git a/old-name.txt b/new-name.txt\nsimilarity index 98%\nrename from old-name.txt\nrename to new-name.txt\nGIT binary patch\nliteral 3\nKcmZQzWC8#H2LJ>B\n\n")

	parsed, err := ParseUnifiedDiff(raw)
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	file := parsed.Files["new-name.txt"]
	if file.Path != "new-name.txt" || file.PreviousPath != "old-name.txt" || !file.Binary {
		t.Fatalf("rename = %#v", file)
	}
	if !bytes.Equal(file.Fragment, raw) {
		t.Fatalf("fragment = %q, want raw", file.Fragment)
	}
}

func TestParseUnifiedDiffAcceptsQuotedPaths(t *testing.T) {
	raw := []byte("diff --git \"a/old\\040name.txt\" \"b/new\\040name.txt\"\nsimilarity index 100%\nrename from old name.txt\nrename to new name.txt\n")

	parsed, err := ParseUnifiedDiff(raw)
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	file := parsed.Files["new name.txt"]
	if file.PreviousPath != "old name.txt" {
		t.Fatalf("rename = %#v", file)
	}
}

func TestParseUnifiedDiffRejectsUntrustworthyInput(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"no sections", "@@ -1 +1 @@\n-x\n+y\n"},
		{"bad header", "diff --git alpha b/alpha\n"},
		{"duplicate path", "diff --git a/a b/a\ndiff --git a/a b/a\n"},
		{"duplicate rename source", "diff --git a/old b/new-one\nrename from old\nrename to new-one\ndiff --git a/old b/new-two\nrename from old\nrename to new-two\n"},
		{"incomplete rename", "diff --git a/old b/new\nrename from old\n"},
		{"ambiguous rename target", "diff --git a/old b/new\nrename from old\nrename to other\n"},
		{"ambiguous old source", "diff --git a/old b/new\n--- a/other\n+++ b/new\n"},
		{"binary marker before section", "GIT binary patch\ndiff --git a/b b/b\n"},
		{"unsupported framing", "diff --cc a\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseUnifiedDiff([]byte(test.raw))
			if !errors.Is(err, ErrMalformedUnifiedDiff) {
				t.Fatalf("ParseUnifiedDiff() error = %v, want ErrMalformedUnifiedDiff", err)
			}
		})
	}
}
