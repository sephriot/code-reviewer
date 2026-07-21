package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	projectmigrations "github.com/sephriot/code-reviewer/migrations"
)

type migration struct {
	version  int
	name     string
	checksum string
	sql      string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(projectmigrations.SQLite, "sqlite")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		separator := strings.IndexByte(entry.Name(), '_')
		if separator < 1 {
			return nil, fmt.Errorf("migration %q has no numeric prefix", entry.Name())
		}
		version, err := strconv.Atoi(entry.Name()[:separator])
		if err != nil || version < 1 {
			return nil, fmt.Errorf("migration %q has invalid version", entry.Name())
		}
		contents, err := fs.ReadFile(projectmigrations.SQLite, "sqlite/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		digest := sha256.Sum256(contents)
		migrations = append(migrations, migration{
			version:  version,
			name:     entry.Name(),
			checksum: hex.EncodeToString(digest[:]),
			sql:      string(contents),
		})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].version < migrations[j].version })
	for index, item := range migrations {
		expected := index + 1
		if item.version != expected {
			return nil, fmt.Errorf("migration history is not contiguous: got version %d, want %d", item.version, expected)
		}
	}
	return migrations, nil
}

// ApplyMigrations applies all embedded migrations and returns versions newly applied.
func (s *Store) ApplyMigrations(ctx context.Context) ([]int, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	if err := s.ensureMigrationTable(ctx); err != nil {
		return nil, err
	}

	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return nil, err
	}
	if _, _, err := validateAppliedMigrations(migrations, applied); err != nil {
		return nil, err
	}

	newlyApplied := make([]int, 0)
	for _, item := range migrations {
		if checksum, exists := applied[item.version]; exists {
			if checksum != item.checksum {
				return nil, fmt.Errorf("migration %d checksum changed", item.version)
			}
			continue
		}
		if err := s.applyMigration(ctx, item); err != nil {
			return nil, err
		}
		newlyApplied = append(newlyApplied, item.version)
	}
	return newlyApplied, nil
}

func (s *Store) ensureMigrationTable(ctx context.Context) error {
	return withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at_us INTEGER NOT NULL
)`)
		return err
	})
}

func (s *Store) appliedMigrations(ctx context.Context) (map[int]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	applied := make(map[int]string)
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return applied, nil
}

func (s *Store) applyMigration(ctx context.Context, item migration) error {
	return withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, item.sql); err != nil {
			return fmt.Errorf("execute migration %d: %w", item.version, err)
		}
		_, err := conn.ExecContext(
			ctx,
			"INSERT INTO schema_migrations(version, name, checksum, applied_at_us) VALUES (?, ?, ?, ?)",
			item.version,
			item.name,
			item.checksum,
			time.Now().UTC().UnixMicro(),
		)
		if err != nil {
			return fmt.Errorf("record migration %d: %w", item.version, err)
		}
		return nil
	})
}

func withImmediateConnection(ctx context.Context, db *sql.DB, operation func(*sql.Conn) error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin immediate transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err = operation(conn); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// SchemaStatus reports applied and available schema versions.
func (s *Store) SchemaStatus(ctx context.Context) (SchemaStatus, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return SchemaStatus{}, err
	}
	latest := 0
	if len(migrations) > 0 {
		latest = migrations[len(migrations)-1].version
	}

	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'").Scan(&exists); err != nil {
		return SchemaStatus{}, fmt.Errorf("check migration table: %w", err)
	}
	if exists == 0 {
		return SchemaStatus{Latest: latest, Pending: len(migrations)}, nil
	}

	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return SchemaStatus{}, err
	}
	current, pending, err := validateAppliedMigrations(migrations, applied)
	if err != nil {
		return SchemaStatus{}, err
	}
	return SchemaStatus{Current: current, Latest: latest, Pending: pending}, nil
}

func validateAppliedMigrations(migrations []migration, applied map[int]string) (int, int, error) {
	known := make(map[int]migration, len(migrations))
	for _, item := range migrations {
		known[item.version] = item
	}
	current := 0
	for version, checksum := range applied {
		item, exists := known[version]
		if !exists {
			return 0, 0, fmt.Errorf("database contains unknown migration version %d", version)
		}
		if checksum != item.checksum {
			return 0, 0, fmt.Errorf("migration %d checksum changed", version)
		}
		if version > current {
			current = version
		}
	}
	for version := 1; version <= current; version++ {
		if _, exists := applied[version]; !exists {
			return 0, 0, fmt.Errorf("database migration history has a gap at version %d", version)
		}
	}
	return current, len(migrations) - len(applied), nil
}
