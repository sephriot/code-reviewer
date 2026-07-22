// Package sqlite provides the local control-plane persistence implementation.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var (
	// ErrLeaseLost means a stale worker no longer owns the job generation.
	ErrLeaseLost = errors.New("job lease lost")
	// ErrNoJob means no job is currently eligible for claim.
	ErrNoJob = errors.New("no job available")
	// ErrNoOutboxDelivery means no outbox delivery is currently eligible for claim.
	ErrNoOutboxDelivery = errors.New("no outbox delivery available")
)

// Store owns a SQLite connection pool.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens a control-plane database without applying migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", dataSourceName(absPath, false))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := os.Chmod(absPath, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure database permissions: %w", err)
	}

	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("check database integrity: %w", err)
	}
	if integrity != "ok" {
		_ = db.Close()
		return nil, fmt.Errorf("database quick_check: %s", integrity)
	}

	return &Store{db: db, path: absPath}, nil
}

// OpenReadOnly opens an existing database without creating files, changing
// journal mode, or modifying permissions.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("stat database: %w", err)
	}
	if info.Size() == 0 {
		return nil, errors.New("database is empty")
	}
	db, err := sql.Open("sqlite", dataSourceName(absPath, true))
	if err != nil {
		return nil, fmt.Errorf("open database read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("check database integrity: %w", err)
	}
	if integrity != "ok" {
		_ = db.Close()
		return nil, fmt.Errorf("database quick_check: %s", integrity)
	}
	return &Store{db: db, path: absPath}, nil
}

func dataSourceName(path string, readOnly bool) string {
	return dataSourceNameOptions(path, readOnly, readOnly)
}

func dataSourceNameOptions(path string, readOnly, queryOnly bool) string {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := u.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		query.Set("mode", "ro")
		if queryOnly {
			query.Add("_pragma", "query_only(1)")
		}
	} else {
		query.Add("_pragma", "journal_mode(WAL)")
	}
	u.RawQuery = query.Encode()
	return u.String()
}

// Close closes all database connections.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Ping verifies database access.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Path returns the resolved database path.
func (s *Store) Path() string { return s.path }

// SchemaStatus describes migration readiness.
type SchemaStatus struct {
	Current int `json:"current"`
	Latest  int `json:"latest"`
	Pending int `json:"pending"`
}

// JobInput describes a durable job.
type JobInput struct {
	Kind         string
	ResourceType string
	ResourceID   string
	DedupeKey    string
	Payload      []byte
	Priority     int
	AvailableAt  time.Time
	MaxAttempts  int
}

// Job is a claimed durable job and its fencing token.
type Job struct {
	ID              string
	Kind            string
	Payload         []byte
	State           string
	Attempt         int
	MaxAttempts     int
	LeaseOwner      string
	LeaseGeneration int64
	LeaseExpiresAt  time.Time
}

// OutboxDelivery is a claimed durable event delivery and its fencing token.
type OutboxDelivery struct {
	ID              string
	EventID         string
	Topic           string
	Payload         []byte
	State           string
	Attempt         int
	MaxAttempts     int
	LeaseOwner      string
	LeaseGeneration int64
	LeaseExpiresAt  time.Time
}
