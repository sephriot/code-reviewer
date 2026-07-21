package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sephriot/code-reviewer/internal/legacy"
)

var (
	// ErrLegacyChecksumConflict means an immutable source row changed after it
	// was first imported under the same source identity.
	ErrLegacyChecksumConflict = errors.New("legacy source row checksum conflict")
	// ErrLegacyImportInProgress means another fenced importer owns this source.
	ErrLegacyImportInProgress = errors.New("legacy import already in progress")
)

const legacyImportLeaseDuration = 2 * time.Minute

var legacyImportTables = []string{
	"own_prs", "pending_approvals", "pr_reviews",
	"review_request_sync_state", "review_requests", "review_started_comments",
}

// LegacyImportInput is a manifest-verified legacy snapshot and its stable
// source identity. Callers must verify the backup manifest before constructing
// this input.
type LegacyImportInput struct {
	SourceID     string
	DisplayName  string
	SourceReport DatabaseReport
	Snapshot     legacy.Snapshot
}

// LegacyImportReport describes the exact effects an import has or would have.
type LegacyImportReport struct {
	SourceID             string `json:"source_id"`
	SnapshotID           string `json:"snapshot_id"`
	SnapshotState        string `json:"snapshot_state"`
	SourceRows           int64  `json:"source_rows"`
	ExistingLedgerRows   int64  `json:"existing_ledger_rows"`
	NewLedgerRows        int64  `json:"new_ledger_rows"`
	PullRequestGroups    int64  `json:"pull_request_groups"`
	RepositoriesToCreate int64  `json:"repositories_to_create"`
	PullRequestsToCreate int64  `json:"pull_requests_to_create"`
	RevisionsToCreate    int64  `json:"revisions_to_create"`
	SyntheticRevisions   int64  `json:"synthetic_revisions"`
	WarningCount         int64  `json:"warning_count"`
	Applied              bool   `json:"applied"`
}

type importPlan struct {
	report     LegacyImportReport
	snapshotID string
	coverage   string
	groups     []legacy.PullRequestGroup
	ungrouped  []legacy.Row
	warnings   map[string][]legacy.Warning
}

type legacyImportLease struct {
	Owner       string `json:"owner"`
	Generation  int64  `json:"generation"`
	ExpiresAtUS int64  `json:"expires_at_us"`
}

// PlanLegacyImport validates a complete import without writing to the target.
func (s *Store) PlanLegacyImport(ctx context.Context, input LegacyImportInput) (LegacyImportReport, error) {
	plan, err := s.planLegacyImport(ctx, input)
	if err != nil {
		return LegacyImportReport{}, err
	}
	return plan.report, nil
}

// ImportLegacy imports every source row without scheduling or publishing work.
func (s *Store) ImportLegacy(ctx context.Context, input LegacyImportInput) (LegacyImportReport, error) {
	if !validSourceID(input.SourceID) {
		return LegacyImportReport{}, errors.New("source ID must contain only letters, digits, '.', '_', ':', or '-'")
	}
	lease, err := s.acquireLegacyImportLease(ctx, input.SourceID)
	if err != nil {
		return LegacyImportReport{}, err
	}
	defer s.releaseLegacyImportLease(input.SourceID, lease)

	plan, err := s.planLegacyImport(ctx, input)
	if err != nil {
		return LegacyImportReport{}, err
	}
	if plan.report.SnapshotState == "complete" && plan.report.NewLedgerRows == 0 {
		return plan.report, nil
	}

	now := time.Now().UTC().UnixMicro()
	if err := s.prepareLegacySnapshot(ctx, input, plan, lease, now); err != nil {
		return LegacyImportReport{}, err
	}
	for _, group := range plan.groups {
		if err := s.importLegacyGroup(ctx, input, plan, group, lease, now); err != nil {
			return LegacyImportReport{}, err
		}
	}
	if len(plan.ungrouped) > 0 {
		if err := s.importLegacyRows(ctx, input, plan, plan.ungrouped, "", "", nil, lease, now); err != nil {
			return LegacyImportReport{}, err
		}
	}
	if err := s.finalizeLegacySnapshot(ctx, input, plan, lease, now); err != nil {
		return LegacyImportReport{}, err
	}
	plan.report.SnapshotState = "complete"
	plan.report.Applied = true
	return plan.report, nil
}

func (s *Store) planLegacyImport(ctx context.Context, input LegacyImportInput) (importPlan, error) {
	plan, err := validateLegacyImportInput(input)
	if err != nil {
		return importPlan{}, err
	}
	status, err := s.SchemaStatus(ctx)
	if err != nil {
		return importPlan{}, err
	}
	if status.Current != 2 || status.Latest != 2 || status.Pending != 0 {
		return importPlan{}, fmt.Errorf("legacy import requires schema version 2: current=%d latest=%d pending=%d", status.Current, status.Latest, status.Pending)
	}
	var publicationMode string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = 'publication_mode'`).Scan(&publicationMode); err != nil {
		return importPlan{}, fmt.Errorf("read publication mode: %w", err)
	}
	if publicationMode != "disabled" {
		return importPlan{}, fmt.Errorf("legacy import requires publication mode disabled, got %q", publicationMode)
	}

	var existingState, physicalSHA, rowsetSHA string
	var rowCount int64
	var verifiedRowCount sql.NullInt64
	var existingCoverage sql.NullString
	err = s.db.QueryRowContext(ctx, `
SELECT state, physical_sha256, rowset_sha256, row_count, verified_row_count, coverage_sha256
FROM legacy_snapshots WHERE id = ?`, plan.snapshotID).Scan(
		&existingState, &physicalSHA, &rowsetSHA, &rowCount, &verifiedRowCount, &existingCoverage,
	)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return importPlan{}, fmt.Errorf("read legacy snapshot: %w", err)
	}
	if err == nil {
		if physicalSHA != input.SourceReport.SHA256 || rowsetSHA != input.Snapshot.RowsetSHA256 || rowCount != input.Snapshot.TotalRows {
			return importPlan{}, errors.New("existing legacy snapshot metadata conflicts with import source")
		}
		plan.report.SnapshotState = existingState
		if existingState == "complete" {
			membership, actualCoverage, err := legacySnapshotCoverage(ctx, s.db, plan.snapshotID)
			if err != nil {
				return importPlan{}, err
			}
			if !verifiedRowCount.Valid || verifiedRowCount.Int64 != input.Snapshot.TotalRows ||
				!existingCoverage.Valid || existingCoverage.String != plan.coverage ||
				actualCoverage != plan.coverage || membership != input.Snapshot.TotalRows {
				return importPlan{}, errors.New("completed legacy snapshot has invalid coverage")
			}
		}
	}

	for _, row := range input.Snapshot.Rows {
		var checksum string
		err := s.db.QueryRowContext(ctx, `
SELECT row_checksum FROM migration_ledger
WHERE source_id = ? AND source_table = ? AND source_pk = ?`,
			input.SourceID, row.Table, strconv.FormatInt(row.ID, 10)).Scan(&checksum)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			plan.report.NewLedgerRows++
		case err != nil:
			return importPlan{}, fmt.Errorf("read migration ledger %s/%d: %w", row.Table, row.ID, err)
		case checksum != row.SHA256:
			return importPlan{}, fmt.Errorf("%w: %s/%d", ErrLegacyChecksumConflict, row.Table, row.ID)
		default:
			plan.report.ExistingLedgerRows++
		}
	}
	if err := s.countPlannedEntities(ctx, input, &plan); err != nil {
		return importPlan{}, err
	}
	return plan, nil
}

func validateLegacyImportInput(input LegacyImportInput) (importPlan, error) {
	if err := legacy.ValidateSnapshot(input.Snapshot); err != nil {
		return importPlan{}, fmt.Errorf("validate legacy snapshot: %w", err)
	}
	if !validSourceID(input.SourceID) {
		return importPlan{}, errors.New("source ID must contain only letters, digits, '.', '_', ':', or '-'")
	}
	if strings.TrimSpace(input.DisplayName) == "" {
		return importPlan{}, errors.New("source display name is required")
	}
	if input.SourceReport.Integrity != "ok" {
		return importPlan{}, errors.New("source integrity is not ok")
	}
	for label, value := range map[string]string{
		"physical SHA": input.SourceReport.SHA256,
		"schema SHA":   input.SourceReport.SchemaSHA256,
		"logical SHA":  input.SourceReport.LogicalSHA256,
		"rowset SHA":   input.Snapshot.RowsetSHA256,
	} {
		if !validHex(value, 64) {
			return importPlan{}, fmt.Errorf("%s is invalid", label)
		}
	}
	if input.SourceReport.SizeBytes <= 0 {
		return importPlan{}, errors.New("source size must be positive")
	}
	if input.Snapshot.RowFormatVersion != 1 {
		return importPlan{}, fmt.Errorf("unsupported legacy row format %d", input.Snapshot.RowFormatVersion)
	}
	if filepath.Clean(input.SourceReport.Path) != filepath.Clean(input.Snapshot.Path) {
		return importPlan{}, errors.New("source report and snapshot paths differ")
	}
	canonicalDigest := sha256.Sum256(input.Snapshot.CanonicalJSON)
	if hex.EncodeToString(canonicalDigest[:]) != input.Snapshot.RowsetSHA256 {
		return importPlan{}, errors.New("legacy canonical rowset checksum mismatch")
	}
	if input.Snapshot.TotalRows != int64(len(input.Snapshot.Rows)) {
		return importPlan{}, errors.New("legacy total row count mismatch")
	}
	if !equalCounts(input.SourceReport.TableCounts, input.Snapshot.TableCounts) {
		return importPlan{}, errors.New("manifest and snapshot table counts differ")
	}

	counts := make(map[string]int64, len(legacyImportTables))
	for _, table := range legacyImportTables {
		counts[table] = 0
	}
	seen := make(map[string]struct{}, len(input.Snapshot.Rows))
	groups := make(map[string][]legacy.Row)
	groupKeys := make([]string, 0)
	ungrouped := make([]legacy.Row, 0)
	warnings := make(map[string][]legacy.Warning)
	for _, warning := range input.Snapshot.Warnings {
		warnings[rowKey(warning.Table, warning.ID)] = append(warnings[rowKey(warning.Table, warning.ID)], warning)
	}
	for _, row := range input.Snapshot.Rows {
		if !knownLegacyTable(row.Table) || row.ID <= 0 {
			return importPlan{}, fmt.Errorf("invalid legacy row identity %s/%d", row.Table, row.ID)
		}
		key := rowKey(row.Table, row.ID)
		if _, exists := seen[key]; exists {
			return importPlan{}, fmt.Errorf("duplicate legacy row %s", key)
		}
		seen[key] = struct{}{}
		counts[row.Table]++
		if !json.Valid(row.RawJSON) || len(row.RawJSON) == 0 || row.RawJSON[0] != '{' {
			return importPlan{}, fmt.Errorf("legacy row %s raw JSON is invalid", key)
		}
		digest := sha256.Sum256(row.RawJSON)
		if hex.EncodeToString(digest[:]) != row.SHA256 {
			return importPlan{}, fmt.Errorf("legacy row %s checksum mismatch", key)
		}
		if row.Repository == "" || row.PRNumber <= 0 {
			if row.Table != "review_request_sync_state" || row.Revision != nil {
				return importPlan{}, fmt.Errorf("legacy row %s has invalid pull-request identity", key)
			}
			ungrouped = append(ungrouped, row)
			continue
		}
		if _, _, ok := splitRepository(row.Repository); !ok || row.Revision == nil || row.Revision.Publishable {
			return importPlan{}, fmt.Errorf("legacy row %s has invalid non-publishable revision", key)
		}
		groupKey := strings.ToLower(row.Repository) + "#" + strconv.FormatInt(row.PRNumber, 10)
		if _, exists := groups[groupKey]; !exists {
			groupKeys = append(groupKeys, groupKey)
		}
		groups[groupKey] = append(groups[groupKey], row)
	}
	if !equalCounts(counts, input.Snapshot.TableCounts) {
		return importPlan{}, errors.New("snapshot rows and table counts differ")
	}
	sort.Strings(groupKeys)
	orderedGroups := make([]legacy.PullRequestGroup, 0, len(groupKeys))
	for _, key := range groupKeys {
		rows := groups[key]
		orderedGroups = append(orderedGroups, legacy.PullRequestGroup{
			Repository: rows[0].Repository, PRNumber: rows[0].PRNumber, Rows: rows,
		})
	}

	coverage := coverageDigest(input.Snapshot.Rows)
	snapshotID := stableID("legacy_snapshot", input.SourceID, input.SourceReport.SchemaSHA256, input.SourceReport.LogicalSHA256)
	return importPlan{
		report: LegacyImportReport{
			SourceID: input.SourceID, SnapshotID: snapshotID, SnapshotState: "planned",
			SourceRows: input.Snapshot.TotalRows, PullRequestGroups: int64(len(orderedGroups)),
			WarningCount: int64(len(input.Snapshot.Warnings)),
		},
		snapshotID: snapshotID, coverage: coverage, groups: orderedGroups,
		ungrouped: ungrouped, warnings: warnings,
	}, nil
}

func (s *Store) countPlannedEntities(ctx context.Context, input LegacyImportInput, plan *importPlan) error {
	repositories := make(map[string]struct{})
	pullRequests := make(map[string]struct{})
	revisions := make(map[string]struct{})
	for _, group := range plan.groups {
		repoID := stableID("repo", strings.ToLower(group.Repository))
		prID := stableID("pr", repoID, strconv.FormatInt(group.PRNumber, 10))
		repositories[repoID] = struct{}{}
		pullRequests[prID] = struct{}{}
		for _, row := range group.Rows {
			kind, err := revisionKind(row.Revision)
			if err != nil {
				return err
			}
			identityKey := revisionIdentityKey(input.SourceID, row.Revision, kind)
			revisions[stableID("revision", prID, kind, identityKey)] = struct{}{}
			if kind == "synthetic_legacy" {
				plan.report.SyntheticRevisions++
			}
		}
	}
	for id := range repositories {
		missing, err := s.missingID(ctx, "repositories", id)
		if err != nil {
			return err
		}
		if missing {
			plan.report.RepositoriesToCreate++
		}
	}
	for id := range pullRequests {
		missing, err := s.missingID(ctx, "pull_requests", id)
		if err != nil {
			return err
		}
		if missing {
			plan.report.PullRequestsToCreate++
		}
	}
	for id := range revisions {
		missing, err := s.missingID(ctx, "revisions", id)
		if err != nil {
			return err
		}
		if missing {
			plan.report.RevisionsToCreate++
		}
	}
	return nil
}

func (s *Store) missingID(ctx context.Context, table, id string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE id = ?", id).Scan(&count); err != nil {
		return false, fmt.Errorf("check %s identity: %w", table, err)
	}
	return count == 0, nil
}

func (s *Store) prepareLegacySnapshot(ctx context.Context, input LegacyImportInput, plan importPlan, lease *legacyImportLease, now int64) error {
	countsJSON, err := json.Marshal(input.Snapshot.TableCounts)
	if err != nil {
		return fmt.Errorf("encode legacy table counts: %w", err)
	}
	return withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		if err := renewLegacyImportLease(ctx, conn, input.SourceID, lease); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `
INSERT INTO legacy_sources(id, source_kind, display_name, location, created_at_us, updated_at_us)
VALUES (?, 'sqlite', ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  display_name = excluded.display_name,
  location = excluded.location,
  updated_at_us = excluded.updated_at_us`,
			input.SourceID, input.DisplayName, input.Snapshot.Path, now, now)
		if err != nil {
			return fmt.Errorf("upsert legacy source: %w", err)
		}
		_, err = conn.ExecContext(ctx, `
INSERT INTO legacy_snapshots(
  id, source_id, physical_sha256, schema_sha256, logical_sha256,
  rowset_sha256, row_format_version, source_size_bytes, table_count,
  row_count, table_counts_json, state, captured_at_us
) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, 'importing', ?) ON CONFLICT(id) DO NOTHING`,
			plan.snapshotID, input.SourceID, input.SourceReport.SHA256,
			input.SourceReport.SchemaSHA256, input.SourceReport.LogicalSHA256,
			input.Snapshot.RowsetSHA256, input.SourceReport.SizeBytes,
			len(input.Snapshot.TableCounts), input.Snapshot.TotalRows, countsJSON, now)
		if err != nil {
			return fmt.Errorf("create legacy snapshot: %w", err)
		}
		var physical, rowset string
		var rowCount int64
		if err := conn.QueryRowContext(ctx, `SELECT physical_sha256, rowset_sha256, row_count FROM legacy_snapshots WHERE id = ?`, plan.snapshotID).Scan(&physical, &rowset, &rowCount); err != nil {
			return fmt.Errorf("verify legacy snapshot identity: %w", err)
		}
		if physical != input.SourceReport.SHA256 || rowset != input.Snapshot.RowsetSHA256 || rowCount != input.Snapshot.TotalRows {
			return errors.New("existing legacy snapshot metadata conflicts with import source")
		}
		return nil
	})
}

func (s *Store) importLegacyGroup(ctx context.Context, input LegacyImportInput, plan importPlan, group legacy.PullRequestGroup, lease *legacyImportLease, now int64) error {
	owner, name, _ := splitRepository(group.Repository)
	repoID := stableID("repo", strings.ToLower(group.Repository))
	prID := stableID("pr", repoID, strconv.FormatInt(group.PRNumber, 10))
	metadata := preferredMetadata(group.Rows)
	return withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		if err := renewLegacyImportLease(ctx, conn, input.SourceID, lease); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `
INSERT INTO repositories(id, full_name, owner_login, name, created_at_us, updated_at_us)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, repoID, group.Repository, owner, name, now, now); err != nil {
			return fmt.Errorf("create repository %s: %w", group.Repository, err)
		}
		if _, err := conn.ExecContext(ctx, `
INSERT INTO pull_requests(id, repository_id, number, title, author_login, html_url, state, created_at_us, updated_at_us)
VALUES (?, ?, ?, ?, ?, ?, 'unknown', ?, ?) ON CONFLICT(id) DO NOTHING`,
			prID, repoID, group.PRNumber, nullIfEmpty(metadata.Title), nullIfEmpty(metadata.Author), nullIfEmpty(metadata.URL), now, now); err != nil {
			return fmt.Errorf("create pull request %s#%d: %w", group.Repository, group.PRNumber, err)
		}
		revisionIDs := make(map[string]string)
		for _, row := range group.Rows {
			kind, err := revisionKind(row.Revision)
			if err != nil {
				return err
			}
			identityKey := revisionIdentityKey(input.SourceID, row.Revision, kind)
			revisionID := stableID("revision", prID, kind, identityKey)
			if _, exists := revisionIDs[kind+"\x00"+identityKey]; !exists {
				if _, err := conn.ExecContext(ctx, `
INSERT INTO revisions(
  id, pull_request_id, identity_kind, identity_key, head_sha, base_sha,
  is_publishable, observed_at_us, created_at_us
) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?) ON CONFLICT(id) DO NOTHING`,
					revisionID, prID, kind, identityKey,
					nullIfEmpty(row.Revision.HeadSHA), nullIfEmpty(row.Revision.BaseSHA), now, now); err != nil {
					return fmt.Errorf("create revision for %s#%d: %w", group.Repository, group.PRNumber, err)
				}
				revisionIDs[kind+"\x00"+identityKey] = revisionID
			}
		}
		return importLegacyRowsConn(ctx, conn, input, plan, group.Rows, repoID, prID, revisionIDs, now)
	})
}

func (s *Store) importLegacyRows(ctx context.Context, input LegacyImportInput, plan importPlan, rows []legacy.Row, repoID, prID string, revisions map[string]string, lease *legacyImportLease, now int64) error {
	return withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		if err := renewLegacyImportLease(ctx, conn, input.SourceID, lease); err != nil {
			return err
		}
		return importLegacyRowsConn(ctx, conn, input, plan, rows, repoID, prID, revisions, now)
	})
}

func importLegacyRowsConn(ctx context.Context, conn *sql.Conn, input LegacyImportInput, plan importPlan, rows []legacy.Row, repoID, prID string, revisions map[string]string, now int64) error {
	for _, row := range rows {
		rowWarnings := plan.warnings[rowKey(row.Table, row.ID)]
		if rowWarnings == nil {
			rowWarnings = []legacy.Warning{}
		}
		warningsJSON, err := json.Marshal(rowWarnings)
		if err != nil {
			return fmt.Errorf("encode warnings for %s/%d: %w", row.Table, row.ID, err)
		}
		var revisionID any
		if row.Revision != nil {
			kind, err := revisionKind(row.Revision)
			if err != nil {
				return err
			}
			identityKey := revisionIdentityKey(input.SourceID, row.Revision, kind)
			revisionID = revisions[kind+"\x00"+identityKey]
		}
		ledgerID := stableID("ledger", input.SourceID, row.Table, strconv.FormatInt(row.ID, 10))
		_, err = conn.ExecContext(ctx, `
INSERT INTO migration_ledger(
  id, source_id, snapshot_id, source_table, source_pk, row_checksum,
  row_format_version, raw_json, warnings_json, repository_id,
  pull_request_id, revision_id, imported_at_us
) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?) ON CONFLICT(source_id, source_table, source_pk) DO NOTHING`,
			ledgerID, input.SourceID, plan.snapshotID, row.Table, strconv.FormatInt(row.ID, 10),
			row.SHA256, row.RawJSON, warningsJSON, nullIfEmpty(repoID), nullIfEmpty(prID), revisionID, now)
		if err != nil {
			return fmt.Errorf("insert migration ledger %s/%d: %w", row.Table, row.ID, err)
		}
		var existingID, checksum string
		if err := conn.QueryRowContext(ctx, `
SELECT id, row_checksum FROM migration_ledger
WHERE source_id = ? AND source_table = ? AND source_pk = ?`,
			input.SourceID, row.Table, strconv.FormatInt(row.ID, 10)).Scan(&existingID, &checksum); err != nil {
			return fmt.Errorf("verify migration ledger %s/%d: %w", row.Table, row.ID, err)
		}
		if checksum != row.SHA256 {
			return fmt.Errorf("%w: %s/%d", ErrLegacyChecksumConflict, row.Table, row.ID)
		}
		if _, err := conn.ExecContext(ctx, `
INSERT INTO legacy_snapshot_rows(snapshot_id, source_id, table_name, source_pk, row_checksum, ledger_id)
VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(snapshot_id, table_name, source_pk) DO NOTHING`,
			plan.snapshotID, input.SourceID, row.Table, strconv.FormatInt(row.ID, 10), row.SHA256, existingID); err != nil {
			return fmt.Errorf("link legacy snapshot row %s/%d: %w", row.Table, row.ID, err)
		}
	}
	return nil
}

func (s *Store) finalizeLegacySnapshot(ctx context.Context, input LegacyImportInput, plan importPlan, lease *legacyImportLease, now int64) error {
	return withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		if err := renewLegacyImportLease(ctx, conn, input.SourceID, lease); err != nil {
			return err
		}
		covered, actualCoverage, err := legacySnapshotCoverage(ctx, conn, plan.snapshotID)
		if err != nil {
			return err
		}
		if covered != input.Snapshot.TotalRows {
			return fmt.Errorf("legacy snapshot coverage is %d rows, want %d", covered, input.Snapshot.TotalRows)
		}
		if actualCoverage != plan.coverage {
			return errors.New("legacy snapshot coverage checksum mismatch")
		}
		_, err = conn.ExecContext(ctx, `
UPDATE legacy_snapshots
SET state = 'complete', completed_at_us = ?, verified_row_count = ?, coverage_sha256 = ?
WHERE id = ?`, now, input.Snapshot.TotalRows, plan.coverage, plan.snapshotID)
		if err != nil {
			return fmt.Errorf("finalize legacy snapshot: %w", err)
		}
		return nil
	})
}

type legacyCoverageQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func legacySnapshotCoverage(ctx context.Context, queryer legacyCoverageQueryer, snapshotID string) (int64, string, error) {
	rows, err := queryer.QueryContext(ctx, `
SELECT table_name, source_pk, row_checksum FROM legacy_snapshot_rows
WHERE snapshot_id = ? ORDER BY table_name, CAST(source_pk AS INTEGER), source_pk`, snapshotID)
	if err != nil {
		return 0, "", fmt.Errorf("read imported snapshot coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var count int64
	hash := sha256.New()
	for rows.Next() {
		var table, primaryKey, checksum string
		if err := rows.Scan(&table, &primaryKey, &checksum); err != nil {
			return 0, "", fmt.Errorf("scan imported snapshot coverage: %w", err)
		}
		_, _ = hash.Write([]byte(table + "\x00" + primaryKey + "\x00" + checksum + "\x00"))
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, "", fmt.Errorf("iterate imported snapshot coverage: %w", err)
	}
	return count, hex.EncodeToString(hash.Sum(nil)), nil
}

type prMetadata struct{ Title, Author, URL string }

func preferredMetadata(rows []legacy.Row) prMetadata {
	ranks := map[string]int{"review_requests": 5, "own_prs": 4, "pending_approvals": 3, "pr_reviews": 2, "review_started_comments": 1}
	var result prMetadata
	var titleRank, authorRank, urlRank int
	for _, row := range rows {
		rank := ranks[row.Table]
		if row.Title != "" && rank > titleRank {
			result.Title, titleRank = row.Title, rank
		}
		if row.Author != "" && rank > authorRank {
			result.Author, authorRank = row.Author, rank
		}
		if row.URL != "" && rank > urlRank {
			result.URL, urlRank = row.URL, rank
		}
	}
	return result
}

func revisionKind(revision *legacy.RevisionIdentity) (string, error) {
	if revision == nil || revision.Publishable {
		return "", errors.New("legacy revision must exist and be non-publishable")
	}
	switch revision.Kind {
	case legacy.RevisionLegacyHeadBase:
		if !validHex(revision.HeadSHA, 40) || !validHex(revision.BaseSHA, 40) {
			return "", errors.New("legacy SHA-pair revision is invalid")
		}
		return "legacy_sha_pair", nil
	case legacy.RevisionLegacySynthetic:
		if revision.Key == "" {
			return "", errors.New("synthetic legacy revision key is empty")
		}
		return "synthetic_legacy", nil
	default:
		return "", fmt.Errorf("unknown legacy revision kind %q", revision.Kind)
	}
}

func revisionIdentityKey(sourceID string, revision *legacy.RevisionIdentity, kind string) string {
	if kind == "synthetic_legacy" {
		return sourceID + ":" + revision.Key
	}
	return revision.Key
}

func (s *Store) acquireLegacyImportLease(ctx context.Context, sourceID string) (*legacyImportLease, error) {
	owner, err := newID("legacy_import")
	if err != nil {
		return nil, err
	}
	lease := &legacyImportLease{Owner: owner}
	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		key := legacyImportLeaseKey(sourceID)
		var raw string
		var previous legacyImportLease
		err := conn.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = ?`, key).Scan(&raw)
		if err == nil {
			if json.Unmarshal([]byte(raw), &previous) != nil {
				return errors.New("legacy import lease is corrupt")
			}
			if previous.ExpiresAtUS > time.Now().UTC().UnixMicro() {
				return ErrLegacyImportInProgress
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read legacy import lease: %w", err)
		}
		lease.Generation = previous.Generation + 1
		lease.ExpiresAtUS = time.Now().UTC().Add(legacyImportLeaseDuration).UnixMicro()
		encoded, err := json.Marshal(lease)
		if err != nil {
			return fmt.Errorf("encode legacy import lease: %w", err)
		}
		_, err = conn.ExecContext(ctx, `
INSERT INTO system_state(key, value, updated_at_us) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at_us = excluded.updated_at_us`,
			key, string(encoded), time.Now().UTC().UnixMicro())
		if err != nil {
			return fmt.Errorf("acquire legacy import lease: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return lease, nil
}

func renewLegacyImportLease(ctx context.Context, conn *sql.Conn, sourceID string, lease *legacyImportLease) error {
	key := legacyImportLeaseKey(sourceID)
	var raw string
	if err := conn.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = ?`, key).Scan(&raw); err != nil {
		return fmt.Errorf("read legacy import lease: %w", err)
	}
	var current legacyImportLease
	if err := json.Unmarshal([]byte(raw), &current); err != nil {
		return errors.New("legacy import lease is corrupt")
	}
	now := time.Now().UTC()
	if current.Owner != lease.Owner || current.Generation != lease.Generation || current.ExpiresAtUS <= now.UnixMicro() {
		return ErrLegacyImportInProgress
	}
	lease.ExpiresAtUS = now.Add(legacyImportLeaseDuration).UnixMicro()
	encoded, err := json.Marshal(lease)
	if err != nil {
		return fmt.Errorf("encode legacy import lease: %w", err)
	}
	result, err := conn.ExecContext(ctx, `
UPDATE system_state SET value = ?, updated_at_us = ?
WHERE key = ? AND value = ?`, string(encoded), now.UnixMicro(), key, raw)
	if err != nil {
		return fmt.Errorf("renew legacy import lease: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("verify legacy import lease renewal: %w", err)
	}
	if changed != 1 {
		return ErrLegacyImportInProgress
	}
	return nil
}

func (s *Store) releaseLegacyImportLease(sourceID string, lease *legacyImportLease) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		var raw string
		key := legacyImportLeaseKey(sourceID)
		if err := conn.QueryRowContext(ctx, `SELECT value FROM system_state WHERE key = ?`, key).Scan(&raw); err != nil {
			return nil
		}
		var current legacyImportLease
		if json.Unmarshal([]byte(raw), &current) != nil || current.Owner != lease.Owner || current.Generation != lease.Generation {
			return nil
		}
		_, err := conn.ExecContext(ctx, `DELETE FROM system_state WHERE key = ? AND value = ?`, key, raw)
		return err
	})
}

func legacyImportLeaseKey(sourceID string) string { return "legacy_import_lock:" + sourceID }

func coverageDigest(rows []legacy.Row) string {
	type item struct{ table, pk, checksum string }
	items := make([]item, 0, len(rows))
	for _, row := range rows {
		items = append(items, item{row.Table, strconv.FormatInt(row.ID, 10), row.SHA256})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].table != items[j].table {
			return items[i].table < items[j].table
		}
		left, _ := strconv.ParseInt(items[i].pk, 10, 64)
		right, _ := strconv.ParseInt(items[j].pk, 10, 64)
		return left < right
	})
	hash := sha256.New()
	for _, item := range items {
		_, _ = hash.Write([]byte(item.table + "\x00" + item.pk + "\x00" + item.checksum + "\x00"))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func stableID(namespace string, values ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(namespace))
	for _, value := range values {
		_, _ = hash.Write([]byte{'\x00'})
		_, _ = hash.Write([]byte(value))
	}
	return namespace + "_" + hex.EncodeToString(hash.Sum(nil))[:32]
}

func rowKey(table string, id int64) string { return table + "/" + strconv.FormatInt(id, 10) }

func validSourceID(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._:-", character) {
			continue
		}
		return false
	}
	return true
}

func validHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func knownLegacyTable(table string) bool {
	for _, known := range legacyImportTables {
		if table == known {
			return true
		}
	}
	return false
}

func splitRepository(fullName string) (string, string, bool) {
	owner, name, ok := strings.Cut(fullName, "/")
	return owner, name, ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
