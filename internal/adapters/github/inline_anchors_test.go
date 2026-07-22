package github

import "testing"

func TestRightSideDiffLinesAcceptsAddedAndContextOnly(t *testing.T) {
	raw := []byte("diff --git a/item.go b/item.go\nindex 1..2 100644\n--- a/item.go\n+++ b/item.go\n@@ -4,3 +4,4 @@\n keep\n-old\n+new\n tail\n")
	parsed, err := ParseUnifiedDiff(raw)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := RightSideDiffLines(parsed)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []int{4, 5, 6} {
		if !IsValidRightSideAnchor(lines, "item.go", line) {
			t.Fatalf("line %d rejected", line)
		}
	}
	if IsValidRightSideAnchor(lines, "item.go", 7) || IsValidRightSideAnchor(lines, "missing.go", 4) {
		t.Fatalf("invalid anchor accepted: %#v", lines)
	}
}

func TestRightSideDiffLinesRejectsMalformedHunk(t *testing.T) {
	parsed := ParsedUnifiedDiff{Files: map[string]ParsedDiffFile{"item.go": {Path: "item.go", Fragment: []byte("diff --git a/item.go b/item.go\n@@ malformed\n")}}}
	if _, err := RightSideDiffLines(parsed); err == nil {
		t.Fatal("malformed hunk accepted")
	}
}
