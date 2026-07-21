// Package canonical creates stable revision identity manifests from complete
// GitHub diff coverage.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const FormatVersion = 1

// FileChange is one fully covered path transition in a pull request diff.
type FileChange struct {
	Path         string
	PreviousPath string
	Status       string
	BaseBlobSHA  string
	HeadBlobSHA  string
	BaseMode     string
	HeadMode     string
	Binary       bool
	Patch        []byte
}

// Input is the complete, verified source needed for canonical identity.
type Input struct {
	HeadSHA  string
	BaseSHA  string
	Complete bool
	Files    []FileChange
}

// Revision is a deterministic, publishable canonical revision identity.
type Revision struct {
	IdentityKey    string
	ManifestSHA256 string
	Manifest       []byte
	EntryCount     int
}

// Build creates a versioned manifest only when every changed path is covered.
func Build(input Input) (Revision, error) {
	if !input.Complete {
		return Revision{}, errors.New("canonical revision requires complete diff coverage")
	}
	if !validSHA(input.HeadSHA) || !validSHA(input.BaseSHA) {
		return Revision{}, errors.New("canonical revision requires exact head and base SHAs")
	}
	entries := make([]manifestEntry, 0, len(input.Files))
	for _, file := range input.Files {
		entry, err := normalizeFile(file)
		if err != nil {
			return Revision{}, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path == entries[j].Path {
			return entries[i].PreviousPath < entries[j].PreviousPath
		}
		return entries[i].Path < entries[j].Path
	})
	for index := 1; index < len(entries); index++ {
		if entries[index-1].Path == entries[index].Path {
			return Revision{}, fmt.Errorf("canonical revision contains duplicate path %q", entries[index].Path)
		}
	}
	manifest, err := json.Marshal(manifest{Version: FormatVersion, HeadSHA: input.HeadSHA, BaseSHA: input.BaseSHA, Files: entries})
	if err != nil {
		return Revision{}, fmt.Errorf("encode canonical manifest: %w", err)
	}
	digest := sha256.Sum256(manifest)
	manifestSHA := hex.EncodeToString(digest[:])
	return Revision{
		IdentityKey:    "canonical_diff:v1:" + input.HeadSHA + ":" + input.BaseSHA + ":" + manifestSHA,
		ManifestSHA256: manifestSHA,
		Manifest:       manifest,
		EntryCount:     len(entries),
	}, nil
}

// Validate accepts only the exact canonical v1 serialization produced by
// Build. It verifies complete entry shape before returning its identity.
func Validate(value []byte) (Revision, error) {
	var decoded manifest
	if err := json.Unmarshal(value, &decoded); err != nil {
		return Revision{}, fmt.Errorf("decode canonical manifest: %w", err)
	}
	if decoded.Version != FormatVersion || !validSHA(decoded.HeadSHA) || !validSHA(decoded.BaseSHA) || decoded.Files == nil {
		return Revision{}, errors.New("canonical manifest envelope is invalid")
	}
	for index, entry := range decoded.Files {
		if err := validateManifestEntry(entry); err != nil {
			return Revision{}, fmt.Errorf("canonical manifest file %d: %w", index, err)
		}
		if index > 0 && decoded.Files[index-1].Path >= entry.Path {
			return Revision{}, errors.New("canonical manifest files are not strictly ordered")
		}
	}
	canonicalBytes, err := json.Marshal(decoded)
	if err != nil {
		return Revision{}, fmt.Errorf("encode canonical manifest: %w", err)
	}
	if !bytes.Equal(canonicalBytes, value) {
		return Revision{}, errors.New("canonical manifest serialization is not normalized")
	}
	digest := sha256.Sum256(canonicalBytes)
	manifestSHA := hex.EncodeToString(digest[:])
	return Revision{IdentityKey: "canonical_diff:v1:" + decoded.HeadSHA + ":" + decoded.BaseSHA + ":" + manifestSHA, ManifestSHA256: manifestSHA, Manifest: canonicalBytes, EntryCount: len(decoded.Files)}, nil
}

type manifest struct {
	Version int             `json:"version"`
	HeadSHA string          `json:"head_sha"`
	BaseSHA string          `json:"base_sha"`
	Files   []manifestEntry `json:"files"`
}

type manifestEntry struct {
	Path         string `json:"path"`
	PreviousPath string `json:"previous_path,omitempty"`
	Status       string `json:"status"`
	BaseBlobSHA  string `json:"base_blob_sha,omitempty"`
	HeadBlobSHA  string `json:"head_blob_sha,omitempty"`
	BaseMode     string `json:"base_mode,omitempty"`
	HeadMode     string `json:"head_mode,omitempty"`
	Binary       bool   `json:"binary"`
	PatchSHA256  string `json:"patch_sha256,omitempty"`
}

func normalizeFile(file FileChange) (manifestEntry, error) {
	if !validPath(file.Path) || (file.PreviousPath != "" && !validPath(file.PreviousPath)) {
		return manifestEntry{}, errors.New("canonical revision contains invalid file path")
	}
	if file.Status != "added" && file.Status != "modified" && file.Status != "removed" && file.Status != "renamed" {
		return manifestEntry{}, errors.New("canonical revision contains invalid file status")
	}
	if file.Status == "renamed" && file.PreviousPath == "" {
		return manifestEntry{}, errors.New("renamed file requires previous path")
	}
	if file.Status != "added" && !validSHA(file.BaseBlobSHA) {
		return manifestEntry{}, errors.New("canonical revision requires base blob SHA")
	}
	if file.Status != "removed" && !validSHA(file.HeadBlobSHA) {
		return manifestEntry{}, errors.New("canonical revision requires head blob SHA")
	}
	if file.Status != "added" && !validMode(file.BaseMode) {
		return manifestEntry{}, errors.New("canonical revision requires base file mode")
	}
	if file.Status != "removed" && !validMode(file.HeadMode) {
		return manifestEntry{}, errors.New("canonical revision requires head file mode")
	}
	if (file.BaseMode != "" && !validMode(file.BaseMode)) || (file.HeadMode != "" && !validMode(file.HeadMode)) {
		return manifestEntry{}, errors.New("canonical revision contains invalid file mode")
	}
	entry := manifestEntry{Path: file.Path, PreviousPath: file.PreviousPath, Status: file.Status, BaseBlobSHA: file.BaseBlobSHA, HeadBlobSHA: file.HeadBlobSHA, BaseMode: file.BaseMode, HeadMode: file.HeadMode, Binary: file.Binary}
	if !file.Binary {
		patch := strings.ReplaceAll(string(file.Patch), "\r\n", "\n")
		digest := sha256.Sum256([]byte(patch))
		entry.PatchSHA256 = hex.EncodeToString(digest[:])
	}
	return entry, nil
}

func validateManifestEntry(entry manifestEntry) error {
	if !validPath(entry.Path) || (entry.PreviousPath != "" && !validPath(entry.PreviousPath)) {
		return errors.New("invalid file path")
	}
	if entry.Status != "added" && entry.Status != "modified" && entry.Status != "removed" && entry.Status != "renamed" {
		return errors.New("invalid file status")
	}
	if entry.Status == "renamed" && entry.PreviousPath == "" {
		return errors.New("renamed file requires previous path")
	}
	if entry.Status != "added" && (!validSHA(entry.BaseBlobSHA) || !validMode(entry.BaseMode)) {
		return errors.New("missing base blob or mode")
	}
	if entry.Status != "removed" && (!validSHA(entry.HeadBlobSHA) || !validMode(entry.HeadMode)) {
		return errors.New("missing head blob or mode")
	}
	if entry.Binary {
		if entry.PatchSHA256 != "" {
			return errors.New("binary file cannot contain patch digest")
		}
	} else if !validDigest(entry.PatchSHA256) {
		return errors.New("text file requires patch digest")
	}
	return nil
}

func validPath(value string) bool {
	return value != "" && !strings.HasPrefix(value, "/") && !strings.Contains(value, "\\") && !strings.Contains(value, "\x00") && !strings.Contains(value, "../") && value != ".."
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

func validSHA(value string) bool {
	if len(value) != 40 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
