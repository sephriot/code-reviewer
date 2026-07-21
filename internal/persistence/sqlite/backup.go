package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	// ErrDestinationExists prevents accidental backup overwrite.
	ErrDestinationExists = errors.New("backup destination already exists")
	// ErrInvalidLegacySchema means required legacy tables are absent or changed incompatibly.
	ErrInvalidLegacySchema = errors.New("invalid legacy schema")
)

// DatabaseReport describes a database without exposing row content.
type DatabaseReport struct {
	Path          string           `json:"path"`
	SizeBytes     int64            `json:"size_bytes"`
	SHA256        string           `json:"sha256"`
	SchemaSHA256  string           `json:"schema_sha256"`
	LogicalSHA256 string           `json:"logical_sha256"`
	Integrity     string           `json:"integrity"`
	TableCounts   map[string]int64 `json:"table_counts"`
}

// BackupManifest proves which logical snapshot was written.
type BackupManifest struct {
	FormatVersion int            `json:"format_version"`
	CreatedAtUTC  string         `json:"created_at_utc"`
	SourceBefore  DatabaseReport `json:"source_before"`
	SourceAfter   DatabaseReport `json:"source_after"`
	Backup        DatabaseReport `json:"backup"`
	ManifestPath  string         `json:"manifest_path"`
}

// InspectLegacy validates and summarizes a legacy code-reviewer database.
func InspectLegacy(ctx context.Context, path string) (DatabaseReport, error) {
	return inspectDatabase(ctx, path, true)
}

// IsLegacyDatabase reports whether any legacy application table exists.
// It is intentionally broader than InspectLegacy so partial/unknown legacy
// schemas cannot accidentally receive v2 migrations.
func IsLegacyDatabase(ctx context.Context, path string) (bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("resolve database path: %w", err)
	}
	info, err := os.Stat(absPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat database: %w", err)
	}
	if info.Size() == 0 {
		return false, nil
	}
	db, err := sql.Open("sqlite", dataSourceName(absPath, true))
	if err != nil {
		return false, fmt.Errorf("open database read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	var count int
	err = db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM sqlite_master
WHERE type = 'table'
  AND name IN ('pr_reviews', 'pending_approvals', 'own_prs',
               'review_requests', 'review_request_sync_state',
               'review_started_comments')`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("detect legacy tables: %w", err)
	}
	return count > 0, nil
}

// BackupLegacy creates and verifies a consistent snapshot and JSON manifest.
func BackupLegacy(ctx context.Context, source, destination string) (manifest BackupManifest, err error) {
	if source == "" || destination == "" {
		return BackupManifest{}, errors.New("source and destination are required")
	}
	absDestination, err := filepath.Abs(destination)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("resolve backup destination: %w", err)
	}
	manifestPath := absDestination + ".manifest.json"
	if pathExists(absDestination) || pathExists(manifestPath) {
		return BackupManifest{}, ErrDestinationExists
	}
	if err := os.MkdirAll(filepath.Dir(absDestination), 0o700); err != nil {
		return BackupManifest{}, fmt.Errorf("create backup directory: %w", err)
	}

	sourceBefore, err := InspectLegacy(ctx, source)
	if err != nil {
		return BackupManifest{}, err
	}
	randomID, err := newID("backup")
	if err != nil {
		return BackupManifest{}, err
	}
	temporary := absDestination + ".partial-" + randomID
	defer func() {
		if err != nil {
			_ = os.Remove(temporary)
		}
	}()

	// VACUUM INTO writes only the destination, but SQLite classifies the command
	// as a write operation. mode=ro protects the source; query_only must be off.
	db, err := sql.Open("sqlite", dataSourceNameOptions(sourceBefore.Path, true, false))
	if err != nil {
		return BackupManifest{}, fmt.Errorf("open source for backup: %w", err)
	}
	db.SetMaxOpenConns(1)
	_, vacuumErr := db.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(temporary, "'", "''")+"'")
	closeErr := db.Close()
	if vacuumErr != nil {
		return BackupManifest{}, fmt.Errorf("create SQLite snapshot: %w", vacuumErr)
	}
	if closeErr != nil {
		return BackupManifest{}, fmt.Errorf("close backup source: %w", closeErr)
	}
	if err := os.Chmod(temporary, 0o600); err != nil {
		return BackupManifest{}, fmt.Errorf("secure backup permissions: %w", err)
	}

	backupReport, err := inspectDatabase(ctx, temporary, true)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("verify backup: %w", err)
	}
	if !equivalentLogicalDatabase(sourceBefore, backupReport) {
		return BackupManifest{}, errors.New("backup logical content differs from source snapshot")
	}
	sourceAfter, err := InspectLegacy(ctx, source)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("reinspect source after backup: %w", err)
	}
	if !equivalentLogicalDatabase(sourceBefore, sourceAfter) {
		return BackupManifest{}, errors.New("source database changed while backup was running")
	}

	if err := copyFileExclusive(temporary, absDestination, 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return BackupManifest{}, ErrDestinationExists
		}
		return BackupManifest{}, fmt.Errorf("publish backup without overwrite: %w", err)
	}
	if err := os.Remove(temporary); err != nil {
		_ = os.Remove(absDestination)
		return BackupManifest{}, fmt.Errorf("remove temporary backup: %w", err)
	}
	backupReport, err = inspectDatabase(ctx, absDestination, true)
	if err != nil {
		_ = os.Remove(absDestination)
		return BackupManifest{}, fmt.Errorf("verify published backup: %w", err)
	}
	manifest = BackupManifest{
		FormatVersion: 1,
		CreatedAtUTC:  time.Now().UTC().Format(time.RFC3339Nano),
		SourceBefore:  sourceBefore,
		SourceAfter:   sourceAfter,
		Backup:        backupReport,
		ManifestPath:  manifestPath,
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		_ = os.Remove(absDestination)
		return BackupManifest{}, fmt.Errorf("encode backup manifest: %w", err)
	}
	manifestTemporary := manifestPath + ".partial-" + randomID
	manifestFile, err := os.OpenFile(manifestTemporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.Remove(absDestination)
		return BackupManifest{}, fmt.Errorf("write backup manifest: %w", err)
	}
	if _, err = manifestFile.Write(append(manifestData, '\n')); err == nil {
		err = manifestFile.Sync()
	}
	closeErr = manifestFile.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(manifestTemporary)
		_ = os.Remove(absDestination)
		return BackupManifest{}, fmt.Errorf("write backup manifest: %w", err)
	}
	if err := os.Link(manifestTemporary, manifestPath); err != nil {
		_ = os.Remove(manifestTemporary)
		_ = os.Remove(absDestination)
		if errors.Is(err, os.ErrExist) {
			return BackupManifest{}, ErrDestinationExists
		}
		return BackupManifest{}, fmt.Errorf("publish backup manifest without overwrite: %w", err)
	}
	if err := os.Remove(manifestTemporary); err != nil {
		_ = os.Remove(manifestPath)
		_ = os.Remove(absDestination)
		return BackupManifest{}, fmt.Errorf("remove temporary backup manifest: %w", err)
	}
	return manifest, nil
}

// VerifyLegacyBackup verifies a manifest and the exact backup it describes.
func VerifyLegacyBackup(ctx context.Context, databasePath, manifestPath string) (BackupManifest, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return BackupManifest{}, fmt.Errorf("read backup manifest: %w", err)
	}
	if len(data) > 1<<20 {
		return BackupManifest{}, errors.New("backup manifest exceeds 1 MiB")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var manifest BackupManifest
	if err := decoder.Decode(&manifest); err != nil {
		return BackupManifest{}, fmt.Errorf("decode backup manifest: %w", err)
	}
	if manifest.FormatVersion != 1 {
		return BackupManifest{}, fmt.Errorf("unsupported backup manifest version %d", manifest.FormatVersion)
	}
	report, err := InspectLegacy(ctx, databasePath)
	if err != nil {
		return BackupManifest{}, err
	}
	if report.SHA256 != manifest.Backup.SHA256 ||
		report.SizeBytes != manifest.Backup.SizeBytes ||
		!equivalentLogicalDatabase(report, manifest.Backup) {
		return BackupManifest{}, errors.New("backup does not match manifest")
	}
	return manifest, nil
}

var legacyTables = []string{
	"own_prs",
	"pending_approvals",
	"pr_reviews",
	"review_request_sync_state",
	"review_requests",
	"review_started_comments",
}

func inspectDatabase(ctx context.Context, path string, requireLegacy bool) (DatabaseReport, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return DatabaseReport{}, fmt.Errorf("resolve database path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return DatabaseReport{}, fmt.Errorf("stat database: %w", err)
	}
	if info.Size() == 0 {
		return DatabaseReport{}, errors.New("database is empty")
	}
	fileHash, err := hashFile(absPath)
	if err != nil {
		return DatabaseReport{}, err
	}

	db, err := sql.Open("sqlite", dataSourceName(absPath, true))
	if err != nil {
		return DatabaseReport{}, fmt.Errorf("open database read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return DatabaseReport{}, fmt.Errorf("check database integrity: %w", err)
	}
	if integrity != "ok" {
		return DatabaseReport{}, fmt.Errorf("database integrity_check: %s", integrity)
	}
	schemaHash, names, err := schemaDigest(ctx, db)
	if err != nil {
		return DatabaseReport{}, err
	}
	if requireLegacy {
		for _, table := range legacyTables {
			if _, exists := names[table]; !exists {
				return DatabaseReport{}, fmt.Errorf("%w: missing table %s", ErrInvalidLegacySchema, table)
			}
		}
	}

	counts := make(map[string]int64, len(legacyTables))
	for _, table := range legacyTables {
		if _, exists := names[table]; !exists {
			continue
		}
		var count int64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			return DatabaseReport{}, fmt.Errorf("count table %s: %w", table, err)
		}
		counts[table] = count
	}
	logicalHash, err := logicalDigest(ctx, db, names)
	if err != nil {
		return DatabaseReport{}, err
	}
	return DatabaseReport{
		Path:          absPath,
		SizeBytes:     info.Size(),
		SHA256:        fileHash,
		SchemaSHA256:  schemaHash,
		LogicalSHA256: logicalHash,
		Integrity:     integrity,
		TableCounts:   counts,
	}, nil
}

func logicalDigest(ctx context.Context, db *sql.DB, names map[string]struct{}) (string, error) {
	hash := sha256.New()
	for _, table := range legacyTables {
		if _, exists := names[table]; !exists {
			continue
		}
		rows, err := db.QueryContext(ctx, `SELECT * FROM "`+table+`" ORDER BY id`)
		if err != nil {
			return "", fmt.Errorf("read logical rows from %s: %w", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			return "", fmt.Errorf("read columns from %s: %w", table, err)
		}
		_, _ = io.WriteString(hash, "table\x00"+table+"\x00"+strings.Join(columns, "\x00")+"\x00")
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for index := range values {
			destinations[index] = &values[index]
		}
		for rows.Next() {
			if err := rows.Scan(destinations...); err != nil {
				_ = rows.Close()
				return "", fmt.Errorf("scan logical row from %s: %w", table, err)
			}
			for _, value := range values {
				writeCanonicalValue(hash, value)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return "", fmt.Errorf("iterate logical rows from %s: %w", table, err)
		}
		if err := rows.Close(); err != nil {
			return "", fmt.Errorf("close logical rows from %s: %w", table, err)
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeCanonicalValue(writer io.Writer, value any) {
	var marker string
	var contents []byte
	switch typed := value.(type) {
	case nil:
		marker = "n"
	case int64:
		marker = "i"
		contents = []byte(strconv.FormatInt(typed, 10))
	case float64:
		marker = "f"
		contents = []byte(strconv.FormatFloat(typed, 'g', -1, 64))
	case bool:
		marker = "b"
		contents = []byte(strconv.FormatBool(typed))
	case []byte:
		marker = "x"
		contents = typed
	case string:
		marker = "s"
		contents = []byte(typed)
	case time.Time:
		marker = "t"
		contents = []byte(typed.UTC().Format(time.RFC3339Nano))
	default:
		marker = "u"
		contents = []byte(fmt.Sprintf("%v", typed))
	}
	_, _ = io.WriteString(writer, marker+strconv.Itoa(len(contents))+":")
	_, _ = writer.Write(contents)
}

func schemaDigest(ctx context.Context, db *sql.DB) (string, map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, `
SELECT type, name, COALESCE(sql, '')
FROM sqlite_master
WHERE name NOT LIKE 'sqlite_%'
ORDER BY type, name`)
	if err != nil {
		return "", nil, fmt.Errorf("read database schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	hash := sha256.New()
	names := make(map[string]struct{})
	for rows.Next() {
		var objectType, name, statement string
		if err := rows.Scan(&objectType, &name, &statement); err != nil {
			return "", nil, fmt.Errorf("scan database schema: %w", err)
		}
		_, _ = io.WriteString(hash, objectType+"\x00"+name+"\x00"+statement+"\x00")
		if objectType == "table" {
			names[name] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("iterate database schema: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), names, nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open database for hashing: %w", err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash database: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func equalCounts(left, right map[string]int64) bool {
	if len(left) != len(right) {
		return false
	}
	keys := make([]string, 0, len(left))
	for key := range left {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if left[key] != right[key] {
			return false
		}
	}
	return true
}

func equivalentLogicalDatabase(left, right DatabaseReport) bool {
	return left.Integrity == "ok" &&
		right.Integrity == "ok" &&
		left.SchemaSHA256 == right.SchemaSHA256 &&
		left.LogicalSHA256 == right.LogicalSHA256 &&
		equalCounts(left.TableCounts, right.TableCounts)
}

func copyFileExclusive(source, destination string, mode os.FileMode) (err error) {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := output.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(destination)
		}
	}()
	if _, err = io.Copy(output, input); err != nil {
		return err
	}
	if err = output.Sync(); err != nil {
		return err
	}
	return nil
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
