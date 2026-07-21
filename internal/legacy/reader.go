package legacy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const rowFormatVersion = 1

var requiredTables = []string{
	"own_prs",
	"pending_approvals",
	"pr_reviews",
	"review_request_sync_state",
	"review_requests",
	"review_started_comments",
}

type tablePayload struct {
	Name string            `json:"name"`
	Rows []json.RawMessage `json:"rows"`
}

type rowsetPayload struct {
	RowFormatVersion int            `json:"row_format_version"`
	Tables           []tablePayload `json:"tables"`
}

type groupKey struct {
	repository string
	prNumber   int64
}

type revisionLookupKey struct {
	groupKey
	headSHA string
}

// ReadSnapshot opens path in SQLite read-only/query-only mode and reads every
// required legacy row in stable table/ID order. It never creates or changes the
// source database.
func ReadSnapshot(ctx context.Context, path string) (Snapshot, error) {
	absPath, err := validateSourcePath(path)
	if err != nil {
		return Snapshot{}, err
	}

	db, err := sql.Open("sqlite", readOnlyDSN(absPath))
	if err != nil {
		return Snapshot{}, fmt.Errorf("open legacy database read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return Snapshot{}, fmt.Errorf("ping legacy database: %w", err)
	}
	var queryOnly int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		return Snapshot{}, fmt.Errorf("verify query-only mode: %w", err)
	}
	if queryOnly != 1 {
		return Snapshot{}, errors.New("legacy database is not query-only")
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, fmt.Errorf("begin legacy snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := requireSchema(ctx, tx); err != nil {
		return Snapshot{}, err
	}

	snapshot := Snapshot{
		Path:             absPath,
		RowFormatVersion: rowFormatVersion,
		TableCounts:      make(map[string]int64, len(requiredTables)),
	}
	payload := rowsetPayload{
		RowFormatVersion: rowFormatVersion,
		Tables:           make([]tablePayload, 0, len(requiredTables)),
	}
	for _, table := range requiredTables {
		rows, rawRows, err := readTable(ctx, tx, table)
		if err != nil {
			return Snapshot{}, err
		}
		snapshot.Rows = append(snapshot.Rows, rows...)
		snapshot.TableCounts[table] = int64(len(rows))
		snapshot.TotalRows += int64(len(rows))
		payload.Tables = append(payload.Tables, tablePayload{Name: table, Rows: rawRows})
	}

	resolveRevisions(&snapshot)
	groupRows(&snapshot)
	canonical, err := json.Marshal(payload)
	if err != nil {
		return Snapshot{}, fmt.Errorf("encode canonical legacy rowset: %w", err)
	}
	digest := sha256.Sum256(canonical)
	snapshot.CanonicalJSON = canonical
	snapshot.RowsetSHA256 = hex.EncodeToString(digest[:])

	if err := tx.Commit(); err != nil {
		return Snapshot{}, fmt.Errorf("finish legacy snapshot: %w", err)
	}
	return snapshot, nil
}

func validateSourcePath(path string) (string, error) {
	if path == "" {
		return "", errors.New("legacy database path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve legacy database path: %w", err)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("stat legacy database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("legacy database path is not a regular file")
	}
	if info.Size() == 0 {
		return "", errors.New("legacy database is empty")
	}
	return absPath, nil
}

func readOnlyDSN(path string) string {
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := u.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "query_only(1)")
	u.RawQuery = query.Encode()
	return u.String()
}

func requireSchema(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return fmt.Errorf("read legacy schema: %w", err)
	}
	defer func() { _ = rows.Close() }()

	present := make(map[string]struct{}, len(requiredTables))
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scan legacy schema: %w", err)
		}
		present[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy schema: %w", err)
	}
	for _, table := range requiredTables {
		if _, ok := present[table]; !ok {
			return fmt.Errorf("%w: missing table %s", ErrInvalidSchema, table)
		}
	}
	return nil
}

type column struct {
	name         string
	preserveText bool
}

func readTable(ctx context.Context, tx *sql.Tx, table string) ([]Row, []json.RawMessage, error) {
	columns, err := tableColumns(ctx, tx, table)
	if err != nil {
		return nil, nil, err
	}
	if !hasColumn(columns, "id") {
		return nil, nil, fmt.Errorf("%w: table %s has no id column", ErrInvalidSchema, table)
	}

	selects := make([]string, 0, len(columns))
	for _, column := range columns {
		identifier := quoteIdentifier(column.name)
		if column.preserveText {
			selects = append(selects, "CAST("+identifier+" AS TEXT) AS "+identifier)
		} else {
			selects = append(selects, identifier)
		}
	}
	query := "SELECT " + strings.Join(selects, ", ") + " FROM " + quoteIdentifier(table) + " ORDER BY id"
	result, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, fmt.Errorf("read legacy table %s: %w", table, err)
	}
	defer func() { _ = result.Close() }()

	rows := make([]Row, 0)
	rawRows := make([]json.RawMessage, 0)
	values := make([]any, len(columns))
	destinations := make([]any, len(columns))
	for index := range values {
		destinations[index] = &values[index]
	}
	for result.Next() {
		if err := result.Scan(destinations...); err != nil {
			return nil, nil, fmt.Errorf("scan legacy row from %s: %w", table, err)
		}
		raw := make(map[string]any, len(columns))
		for index, column := range columns {
			raw[column.name] = canonicalValue(values[index])
		}
		rawJSON, err := json.Marshal(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("encode legacy row from %s: %w", table, err)
		}
		row, err := extractRow(table, raw, rawJSON)
		if err != nil {
			return nil, nil, err
		}
		rows = append(rows, row)
		rawRows = append(rawRows, rawJSON)
	}
	if err := result.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate legacy rows from %s: %w", table, err)
	}
	return rows, rawRows, nil
}

func tableColumns(ctx context.Context, tx *sql.Tx, table string) ([]column, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info("+quoteIdentifier(table)+")")
	if err != nil {
		return nil, fmt.Errorf("read columns from %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	columns := make([]column, 0)
	for rows.Next() {
		var position, notNull, primaryKey int
		var name, declaredType string
		var defaultValue any
		if err := rows.Scan(&position, &name, &declaredType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("scan columns from %s: %w", table, err)
		}
		upperType := strings.ToUpper(declaredType)
		columns = append(columns, column{
			name:         name,
			preserveText: strings.Contains(upperType, "DATE") || strings.Contains(upperType, "TIME"),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate columns from %s: %w", table, err)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("%w: table %s has no columns", ErrInvalidSchema, table)
	}
	return columns, nil
}

func hasColumn(columns []column, name string) bool {
	for _, column := range columns {
		if column.name == name {
			return true
		}
	}
	return false
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func canonicalValue(value any) any {
	switch typed := value.(type) {
	case []byte:
		return map[string]string{"$blob_hex": hex.EncodeToString(typed)}
	case time.Time:
		return typed.Format(time.RFC3339Nano)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return map[string]string{"$float": strconv.FormatFloat(typed, 'g', -1, 64)}
		}
		return typed
	default:
		return typed
	}
}

func extractRow(table string, raw map[string]any, rawJSON json.RawMessage) (Row, error) {
	id, ok := integerValue(raw["id"])
	if !ok {
		return Row{}, fmt.Errorf("%w: table %s contains invalid id", ErrInvalidSchema, table)
	}
	digest := sha256.Sum256(rawJSON)
	return Row{
		Table:      table,
		ID:         id,
		Repository: stringValue(raw["repository"]),
		PRNumber:   integerValueOrZero(raw["pr_number"]),
		Title:      stringValue(raw["pr_title"]),
		Author:     stringValue(raw["pr_author"]),
		URL:        stringValue(raw["pr_url"]),
		HeadSHA:    stringValue(raw["head_sha"]),
		BaseSHA:    stringValue(raw["base_sha"]),
		Status:     stringValue(raw["status"]),
		Action:     stringValue(raw["review_action"]),
		RawJSON:    rawJSON,
		SHA256:     hex.EncodeToString(digest[:]),
	}, nil
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return ""
	}
}

func integerValueOrZero(value any) int64 {
	result, _ := integerValue(value)
	return result
}

func integerValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		if typed == math.Trunc(typed) {
			return int64(typed), true
		}
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		return parsed, err == nil
	}
	return 0, false
}

func resolveRevisions(snapshot *Snapshot) {
	candidates := make(map[revisionLookupKey]map[string]struct{})
	for _, row := range snapshot.Rows {
		if row.Repository == "" || row.PRNumber <= 0 {
			continue
		}
		headSHA, validHead := normalizedSHA(row.HeadSHA)
		baseSHA, validBase := normalizedSHA(row.BaseSHA)
		if !validHead || !validBase {
			continue
		}
		key := revisionLookupKey{groupKey: groupKey{repository: row.Repository, prNumber: row.PRNumber}, headSHA: headSHA}
		if candidates[key] == nil {
			candidates[key] = make(map[string]struct{})
		}
		candidates[key][baseSHA] = struct{}{}
	}

	for index := range snapshot.Rows {
		row := &snapshot.Rows[index]
		if row.Repository == "" || row.PRNumber <= 0 {
			snapshot.Warnings = append(snapshot.Warnings, Warning{
				Code: "ungrouped_row", Table: row.Table, ID: row.ID,
				Message: "row has no pull-request identity",
			})
			continue
		}
		headSHA, validHead := normalizedSHA(row.HeadSHA)
		baseSHA, validBase := normalizedSHA(row.BaseSHA)
		resolved := false
		if validHead && !validBase {
			key := revisionLookupKey{groupKey: groupKey{repository: row.Repository, prNumber: row.PRNumber}, headSHA: headSHA}
			if len(candidates[key]) == 1 {
				for candidate := range candidates[key] {
					baseSHA = candidate
				}
				validBase = true
				resolved = true
			}
		}
		if validHead && validBase {
			row.Revision = &RevisionIdentity{
				Kind:        RevisionLegacyHeadBase,
				Key:         "legacy:" + headSHA + ":" + baseSHA,
				HeadSHA:     headSHA,
				BaseSHA:     baseSHA,
				Publishable: false,
			}
			if resolved {
				snapshot.Warnings = append(snapshot.Warnings, Warning{
					Code: "resolved_base_sha", Table: row.Table, ID: row.ID,
					Message: "base SHA resolved from another row with the same pull request and head SHA",
				})
			}
			continue
		}
		row.Revision = &RevisionIdentity{
			Kind:        RevisionLegacySynthetic,
			Key:         "legacy:" + row.Table + ":" + strconv.FormatInt(row.ID, 10),
			Publishable: false,
		}
		snapshot.Warnings = append(snapshot.Warnings, Warning{
			Code: "synthetic_revision", Table: row.Table, ID: row.ID,
			Message: "row lacks one unambiguous exact 40-hex commit pair",
		})
	}
}

func normalizedSHA(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) != 40 {
		return "", false
	}
	for _, character := range trimmed {
		if !((character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'f') ||
			(character >= 'A' && character <= 'F')) {
			return "", false
		}
	}
	return strings.ToLower(trimmed), true
}

func groupRows(snapshot *Snapshot) {
	grouped := make(map[groupKey][]Row)
	keys := make([]groupKey, 0)
	for _, row := range snapshot.Rows {
		if row.Repository == "" || row.PRNumber <= 0 {
			continue
		}
		key := groupKey{repository: row.Repository, prNumber: row.PRNumber}
		if _, exists := grouped[key]; !exists {
			keys = append(keys, key)
		}
		grouped[key] = append(grouped[key], row)
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].repository == keys[right].repository {
			return keys[left].prNumber < keys[right].prNumber
		}
		return keys[left].repository < keys[right].repository
	})
	snapshot.Groups = make([]PullRequestGroup, 0, len(keys))
	for _, key := range keys {
		snapshot.Groups = append(snapshot.Groups, PullRequestGroup{
			Repository: key.repository,
			PRNumber:   key.prNumber,
			Rows:       grouped[key],
		})
	}
}
