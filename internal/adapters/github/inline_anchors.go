package github

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
)

// ErrInvalidInlineAnchor means an anchor is malformed or does not identify a
// current right-side diff line. Callers must downgrade it to review-body text,
// never send it to GitHub as an inline review comment.
var ErrInvalidInlineAnchor = errors.New("invalid inline diff anchor")

// RightSideDiffLines returns every GitHub-postable right-side line in parsed
// diff. Added and context lines are valid; removed lines are deliberately
// excluded. Binary and metadata-only files contribute no anchors.
func RightSideDiffLines(parsed ParsedUnifiedDiff) (map[string]map[int]struct{}, error) {
	lines := make(map[string]map[int]struct{}, len(parsed.Files))
	for path, file := range parsed.Files {
		if path != file.Path || file.Binary {
			continue
		}
		valid, err := rightSideLines(file.Fragment)
		if err != nil {
			return nil, err
		}
		if len(valid) > 0 {
			lines[path] = valid
		}
	}
	return lines, nil
}

// IsValidRightSideAnchor reports whether path and line remain postable on
// GitHub's RIGHT side for this exact unified diff.
func IsValidRightSideAnchor(lines map[string]map[int]struct{}, path string, line int) bool {
	if strings.TrimSpace(path) == "" || line < 1 {
		return false
	}
	_, ok := lines[path][line]
	return ok
}

func rightSideLines(fragment []byte) (map[int]struct{}, error) {
	if len(fragment) == 0 || bytes.IndexByte(fragment, 0) >= 0 {
		return nil, ErrMalformedUnifiedDiff
	}
	valid := make(map[int]struct{})
	var nextLine int
	inHunk := false
	for _, raw := range bytes.Split(fragment, []byte{'\n'}) {
		line := strings.TrimSuffix(string(raw), "\r")
		if strings.HasPrefix(line, "@@ ") {
			value, err := hunkNewStart(line)
			if err != nil {
				return nil, err
			}
			nextLine, inHunk = value, true
			continue
		}
		if !inHunk || line == "" || strings.HasPrefix(line, "\\ No newline at end of file") {
			continue
		}
		switch line[0] {
		case '+', ' ':
			valid[nextLine] = struct{}{}
			nextLine++
		case '-':
			// Deleted left-side line; no right-side increment.
		default:
			return nil, ErrMalformedUnifiedDiff
		}
	}
	return valid, nil
}

func hunkNewStart(line string) (int, error) {
	end := strings.Index(line, " @@")
	if end < 0 {
		return 0, ErrMalformedUnifiedDiff
	}
	parts := strings.Fields(line[3:end])
	if len(parts) != 2 || !strings.HasPrefix(parts[1], "+") {
		return 0, ErrMalformedUnifiedDiff
	}
	coordinate := strings.TrimPrefix(parts[1], "+")
	start, _, found := strings.Cut(coordinate, ",")
	if !found {
		start = coordinate
	}
	value, err := strconv.Atoi(start)
	if err != nil || value < 0 {
		return 0, ErrMalformedUnifiedDiff
	}
	return value, nil
}
