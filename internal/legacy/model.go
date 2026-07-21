// Package legacy reads the retired Python application's SQLite database.
package legacy

import (
	"encoding/json"
	"errors"
)

var (
	// ErrInvalidSchema means the source does not contain every required legacy table.
	ErrInvalidSchema = errors.New("invalid legacy schema")
)

// RevisionKind describes how a legacy row is associated with a revision.
type RevisionKind string

const (
	RevisionLegacyHeadBase  RevisionKind = "legacy_sha_pair"
	RevisionLegacySynthetic RevisionKind = "legacy_synthetic"
)

// RevisionIdentity is a non-publishable legacy revision reference.
type RevisionIdentity struct {
	Kind        RevisionKind `json:"kind"`
	Key         string       `json:"key"`
	HeadSHA     string       `json:"head_sha,omitempty"`
	BaseSHA     string       `json:"base_sha,omitempty"`
	Publishable bool         `json:"publishable"`
}

// Row is one distinct source row and its commonly used fields.
type Row struct {
	Table      string            `json:"table"`
	ID         int64             `json:"id"`
	Repository string            `json:"repository,omitempty"`
	PRNumber   int64             `json:"pr_number,omitempty"`
	Title      string            `json:"title,omitempty"`
	Author     string            `json:"author,omitempty"`
	URL        string            `json:"url,omitempty"`
	HeadSHA    string            `json:"head_sha,omitempty"`
	BaseSHA    string            `json:"base_sha,omitempty"`
	Status     string            `json:"status,omitempty"`
	Action     string            `json:"action,omitempty"`
	Revision   *RevisionIdentity `json:"revision,omitempty"`
	RawJSON    json.RawMessage   `json:"raw_json"`
	SHA256     string            `json:"sha256"`
}

// PullRequestGroup retains all source rows associated with one pull request.
type PullRequestGroup struct {
	Repository string `json:"repository"`
	PRNumber   int64  `json:"pr_number"`
	Rows       []Row  `json:"rows"`
}

// Warning identifies source data that needs importer attention.
type Warning struct {
	Code    string `json:"code"`
	Table   string `json:"table"`
	ID      int64  `json:"id"`
	Message string `json:"message"`
}

// Snapshot is a deterministic, read-only view of every legacy row.
type Snapshot struct {
	Path             string             `json:"path"`
	RowFormatVersion int                `json:"row_format_version"`
	Rows             []Row              `json:"rows"`
	Groups           []PullRequestGroup `json:"groups"`
	TableCounts      map[string]int64   `json:"table_counts"`
	TotalRows        int64              `json:"total_rows"`
	Warnings         []Warning          `json:"warnings"`
	CanonicalJSON    json.RawMessage    `json:"canonical_json"`
	RowsetSHA256     string             `json:"rowset_sha256"`
}
