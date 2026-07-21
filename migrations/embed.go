// Package migrations exposes immutable schema migrations embedded in the binaries.
package migrations

import "embed"

// SQLite contains the ordered SQLite migration files.
//
//go:embed sqlite/*.sql
var SQLite embed.FS
