// Package ownership provides a local single-writer guard for GitHub mutation
// credentials. It is deliberately separate from either application database.
package ownership

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

var ErrHeld = errors.New("writer ownership is held by another process")

// Guard holds an advisory lock and matching durable ownership generation.
// Call Valid immediately before loading mutation credentials and every write.
type Guard struct {
	file       *os.File
	db         *sql.DB
	owner      string
	generation int64
}

// Acquire locks stateDir for this process then increments its singleton owner
// generation in an independent SQLite database.
func Acquire(ctx context.Context, stateDir, owner, checkpoint string, now time.Time) (*Guard, error) {
	stateDir = strings.TrimSpace(stateDir)
	owner = strings.TrimSpace(owner)
	if stateDir == "" || owner == "" || now.IsZero() {
		return nil, errors.New("ownership state directory, owner, and time are required")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create ownership state directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(stateDir, "writer.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open ownership lock: %w", err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrHeld
		}
		return nil, fmt.Errorf("lock ownership state: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(stateDir, "writer-ownership.db"))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("open ownership database: %w", err)
	}
	db.SetMaxOpenConns(1)
	guard := &Guard{file: lock, db: db, owner: owner}
	if err := guard.acquire(ctx, checkpoint, now.UTC()); err != nil {
		_ = guard.Close()
		return nil, err
	}
	return guard, nil
}

func (g *Guard) acquire(ctx context.Context, checkpoint string, now time.Time) error {
	if _, err := g.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS writer_ownership (singleton INTEGER PRIMARY KEY CHECK (singleton = 1), generation INTEGER NOT NULL, owner TEXT NOT NULL, checkpoint TEXT NOT NULL, acquired_at_us INTEGER NOT NULL, heartbeat_at_us INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create ownership schema: %w", err)
	}
	conn, err := g.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin ownership acquisition: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	var generation int64
	err = conn.QueryRowContext(ctx, `SELECT generation FROM writer_ownership WHERE singleton = 1`).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		generation = 1
		_, err = conn.ExecContext(ctx, `INSERT INTO writer_ownership(singleton, generation, owner, checkpoint, acquired_at_us, heartbeat_at_us) VALUES (1, ?, ?, ?, ?, ?)`, generation, g.owner, checkpoint, now.UnixMicro(), now.UnixMicro())
	} else if err == nil {
		generation++
		_, err = conn.ExecContext(ctx, `UPDATE writer_ownership SET generation = ?, owner = ?, checkpoint = ?, acquired_at_us = ?, heartbeat_at_us = ? WHERE singleton = 1`, generation, g.owner, checkpoint, now.UnixMicro(), now.UnixMicro())
	}
	if err != nil {
		return fmt.Errorf("record ownership acquisition: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit ownership acquisition: %w", err)
	}
	committed = true
	g.generation = generation
	return nil
}

// Valid fails closed if the durable owner tuple no longer matches this guard.
func (g *Guard) Valid(ctx context.Context) error {
	if g == nil || g.file == nil || g.db == nil {
		return errors.New("writer ownership guard is closed")
	}
	var owner string
	var generation int64
	if err := g.db.QueryRowContext(ctx, `SELECT owner, generation FROM writer_ownership WHERE singleton = 1`).Scan(&owner, &generation); err != nil {
		return fmt.Errorf("read ownership: %w", err)
	}
	if owner != g.owner || generation != g.generation {
		return errors.New("writer ownership generation is stale")
	}
	return nil
}

func (g *Guard) Close() error {
	if g == nil {
		return nil
	}
	var result error
	if g.db != nil {
		result = g.db.Close()
	}
	if g.file != nil {
		_ = unix.Flock(int(g.file.Fd()), unix.LOCK_UN)
		if err := g.file.Close(); result == nil {
			result = err
		}
	}
	g.db = nil
	g.file = nil
	return result
}
