package github

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrMalformedUnifiedDiff means the input cannot safely identify one
// unambiguous set of changed files. Callers must not infer missing facts from
// this error.
var ErrMalformedUnifiedDiff = errors.New("malformed unified diff")

// ParsedDiffFile is one exact file section from a unified diff. Fragment is
// byte-for-byte identical to the corresponding range of the source response.
// Path is the destination path from the Git header; PreviousPath is set only
// for a verified rename. Binary reports either Git binary representation.
type ParsedDiffFile struct {
	Path         string
	PreviousPath string
	Binary       bool
	Fragment     []byte
}

// ParsedUnifiedDiff indexes each exact file fragment by its destination path.
// A parser never emits duplicate or ambiguous paths.
type ParsedUnifiedDiff struct {
	Files map[string]ParsedDiffFile
}

type diffSection struct {
	start     int
	path      string
	headerOld string
	previous  string
	binary    bool
	oldPatch  string
	newPatch  string
	hasOld    bool
	hasNew    bool
	hasFrom   bool
	hasTo     bool
}

// ParseUnifiedDiff splits a Git unified diff into unambiguous, exact file
// fragments. It intentionally accepts only Git's diff --git framing: malformed
// headers, incomplete renames, duplicate destinations, and contradictory patch
// paths fail closed.
func ParseUnifiedDiff(raw []byte) (ParsedUnifiedDiff, error) {
	if len(raw) == 0 || bytes.IndexByte(raw, 0) >= 0 {
		return ParsedUnifiedDiff{}, malformedDiff("empty or NUL-containing input")
	}

	sections, err := scanDiffSections(raw)
	if err != nil {
		return ParsedUnifiedDiff{}, err
	}
	files := make(map[string]ParsedDiffFile, len(sections))
	renameSources := make(map[string]string)
	for index := range sections {
		section := &sections[index]
		if err := section.validate(); err != nil {
			return ParsedUnifiedDiff{}, err
		}
		if _, exists := files[section.path]; exists {
			return ParsedUnifiedDiff{}, malformedDiff("duplicate destination path %q", section.path)
		}
		if section.previous != "" {
			if destination, exists := renameSources[section.previous]; exists {
				return ParsedUnifiedDiff{}, malformedDiff("rename source %q also targets %q", section.previous, destination)
			}
			renameSources[section.previous] = section.path
		}
		end := len(raw)
		if index+1 < len(sections) {
			end = sections[index+1].start
		}
		files[section.path] = ParsedDiffFile{
			Path:         section.path,
			PreviousPath: section.previous,
			Binary:       section.binary,
			Fragment:     raw[section.start:end],
		}
	}
	return ParsedUnifiedDiff{Files: files}, nil
}

func scanDiffSections(raw []byte) ([]diffSection, error) {
	var sections []diffSection
	for offset := 0; offset < len(raw); {
		next := bytes.IndexByte(raw[offset:], '\n')
		lineEnd := len(raw)
		if next >= 0 {
			lineEnd = offset + next
		}
		line := raw[offset:lineEnd]
		if bytes.HasPrefix(line, []byte("diff --git ")) {
			oldPath, newPath, err := parseDiffHeader(string(line))
			if err != nil {
				return nil, err
			}
			sections = append(sections, diffSection{start: offset, path: newPath, headerOld: oldPath})
		} else if bytes.HasPrefix(line, []byte("diff --")) {
			return nil, malformedDiff("unsupported diff framing %q", line)
		} else if len(sections) == 0 {
			return nil, malformedDiff("content before first diff header")
		} else if err := sections[len(sections)-1].consume(string(line)); err != nil {
			return nil, err
		}
		if next < 0 {
			break
		}
		offset = lineEnd + 1
	}
	if len(sections) == 0 {
		return nil, malformedDiff("no diff headers")
	}
	return sections, nil
}

func (section *diffSection) consume(line string) error {
	switch {
	case strings.HasPrefix(line, "rename from "):
		if section.hasFrom {
			return malformedDiff("duplicate rename source")
		}
		path, err := parseMetadataPath(strings.TrimPrefix(line, "rename from "))
		if err != nil {
			return err
		}
		section.previous, section.hasFrom = path, true
	case strings.HasPrefix(line, "rename to "):
		if section.hasTo {
			return malformedDiff("duplicate rename destination")
		}
		path, err := parseMetadataPath(strings.TrimPrefix(line, "rename to "))
		if err != nil {
			return err
		}
		if path != section.path {
			return malformedDiff("rename destination %q does not match header %q", path, section.path)
		}
		section.hasTo = true
	case strings.HasPrefix(line, "--- "):
		if section.hasOld {
			return malformedDiff("duplicate old patch path")
		}
		path, err := parsePatchPath(strings.TrimPrefix(line, "--- "))
		if err != nil {
			return err
		}
		section.oldPatch, section.hasOld = path, true
	case strings.HasPrefix(line, "+++ "):
		if section.hasNew {
			return malformedDiff("duplicate new patch path")
		}
		path, err := parsePatchPath(strings.TrimPrefix(line, "+++ "))
		if err != nil {
			return err
		}
		section.newPatch, section.hasNew = path, true
	case line == "GIT binary patch" || strings.HasPrefix(line, "Binary files "):
		section.binary = true
	}
	return nil
}

func (section diffSection) validate() error {
	if section.hasFrom != section.hasTo {
		return malformedDiff("incomplete rename metadata for %q", section.path)
	}
	if section.hasFrom && section.previous == section.path {
		return malformedDiff("rename source and destination are both %q", section.path)
	}
	if section.hasOld != section.hasNew {
		return malformedDiff("incomplete patch paths for %q", section.path)
	}
	if section.hasOld {
		if section.oldPatch != "/dev/null" && section.oldPatch != section.previousPath() {
			return malformedDiff("old patch path %q does not match header", section.oldPatch)
		}
		if section.newPatch != "/dev/null" && section.newPatch != section.path {
			return malformedDiff("new patch path %q does not match header", section.newPatch)
		}
	}
	return nil
}

func (section diffSection) previousPath() string {
	if section.previous != "" {
		return section.previous
	}
	return section.headerOld
}

func parseDiffHeader(line string) (string, string, error) {
	rest := strings.TrimPrefix(line, "diff --git ")
	oldToken, rest, err := nextPathToken(rest)
	if err != nil {
		return "", "", err
	}
	newToken, rest, err := nextPathToken(rest)
	if err != nil || rest != "" {
		return "", "", malformedDiff("invalid diff header %q", line)
	}
	oldPath, err := trimGitPrefix(oldToken, "a/")
	if err != nil {
		return "", "", err
	}
	newPath, err := trimGitPrefix(newToken, "b/")
	if err != nil {
		return "", "", err
	}
	return oldPath, newPath, nil
}

func nextPathToken(input string) (string, string, error) {
	if input == "" {
		return "", "", malformedDiff("missing diff path")
	}
	if input[0] == '"' {
		end := 1
		escaped := false
		for ; end < len(input); end++ {
			if !escaped && input[end] == '"' {
				break
			}
			if !escaped && input[end] == '\\' {
				escaped = true
				continue
			}
			escaped = false
		}
		if end == len(input) {
			return "", "", malformedDiff("unterminated quoted diff path")
		}
		value, err := strconv.Unquote(input[:end+1])
		if err != nil {
			return "", "", malformedDiff("invalid quoted diff path")
		}
		if end+1 == len(input) {
			return value, "", nil
		}
		if input[end+1] != ' ' {
			return "", "", malformedDiff("invalid quoted diff path suffix")
		}
		return value, input[end+2:], nil
	}
	space := strings.IndexByte(input, ' ')
	if space < 0 {
		return input, "", nil
	}
	return input[:space], input[space+1:], nil
}

func parseMetadataPath(input string) (string, error) {
	path := input
	if strings.HasPrefix(input, "\"") {
		var rest string
		var err error
		path, rest, err = nextPathToken(input)
		if err != nil || rest != "" {
			return "", malformedDiff("invalid path metadata %q", input)
		}
	}
	if err := validateRelativePath(path); err != nil {
		return "", err
	}
	return path, nil
}

func parsePatchPath(input string) (string, error) {
	path := strings.SplitN(input, "\t", 2)[0]
	if path == "/dev/null" {
		return path, nil
	}
	value, rest, err := nextPathToken(path)
	if err != nil || rest != "" {
		return "", malformedDiff("invalid patch path %q", input)
	}
	if strings.HasPrefix(value, "a/") {
		return trimGitPrefix(value, "a/")
	}
	return trimGitPrefix(value, "b/")
}

func trimGitPrefix(path, prefix string) (string, error) {
	if !strings.HasPrefix(path, prefix) {
		return "", malformedDiff("path %q lacks %q prefix", path, prefix)
	}
	path = strings.TrimPrefix(path, prefix)
	if err := validateRelativePath(path); err != nil {
		return "", err
	}
	return path, nil
}

func validateRelativePath(path string) error {
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") || strings.IndexByte(path, 0) >= 0 {
		return malformedDiff("invalid relative path %q", path)
	}
	for _, component := range strings.Split(path, "/") {
		if component == "" || component == "." || component == ".." {
			return malformedDiff("invalid relative path %q", path)
		}
	}
	return nil
}

func malformedDiff(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformedUnifiedDiff, fmt.Sprintf(format, args...))
}
