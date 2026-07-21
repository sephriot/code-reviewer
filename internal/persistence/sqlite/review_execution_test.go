package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoadCurrentCanonicalReviewTargetReturnsVerifiedSelectedEvidence(t *testing.T) {
	ctx := context.Background()
	store, attached := seedCurrentCanonicalReviewTarget(t, ctx)

	target, err := store.LoadCurrentCanonicalReviewTarget(ctx, "connection-1", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if target.ConnectionID != "connection-1" || target.PullRequestID != "pr-1" ||
		target.ObservationID != "observation-1" || target.RevisionID != attached.RevisionID ||
		target.ManifestID != attached.ManifestID || target.ManifestSHA256 == "" || len(target.ManifestJSON) == 0 {
		t.Fatalf("target = %+v", target)
	}
	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func seedCurrentCanonicalReviewTarget(t *testing.T, ctx context.Context) (*Store, CanonicalRevisionResult) {
	t.Helper()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedMetadataObservation(t, ctx, store.db, "pr-1", "observation-1", projectionHeadSHA, projectionBaseSHA, 10)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id,
 current_observation_id, freshness, updated_at_us)
VALUES ('pr-1', 'repo-1', 'connection-1', NULL, 'observation-1', 'fresh', 10)`); err != nil {
		t.Fatal(err)
	}
	revision := testCanonicalRevision(t)
	attached, err := store.AttachCanonicalRevision(ctx, CanonicalRevisionInput{
		ConnectionID: "connection-1", ObservationID: "observation-1",
		HeadSHA: projectionHeadSHA, BaseSHA: projectionBaseSHA,
		IdentityKey: revision.IdentityKey, ManifestSHA256: revision.ManifestSHA256,
		ManifestJSON: revision.Manifest, EntryCount: 1, AttachedAt: time.Unix(20, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, attached
}

func TestPrepareReviewRunCreatesEvidenceBoundLedger(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	seedReviewProfileVersion(t, ctx, store, "profile-1", "profile-version-1")

	result, err := store.PrepareReviewRun(ctx, testPrepareReviewRunInput())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.IntentID == "" || result.RunID == "" || result.RunContextID == "" || result.IdempotencyKey == "" {
		t.Fatalf("result = %+v", result)
	}
	for _, table := range []string{"review_intents", "review_runs", "review_run_contexts"} {
		assertTableCount(t, ctx, store.db, table, 1)
	}
	assertTableCount(t, ctx, store.db, "review_run_events", 2)
	for _, table := range []string{"jobs", "domain_events", "outbox", "assessments"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}

	rows, err := store.db.QueryContext(ctx, `
SELECT event_kind FROM review_run_events WHERE run_id = ? ORDER BY sequence`, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var events []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		events = append(events, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "queued,preparing" {
		t.Fatalf("events = %v", events)
	}
	var manifestSHA string
	var manifestJSON []byte
	if err := store.db.QueryRowContext(ctx, `
SELECT manifest_sha256, manifest_json FROM review_run_contexts WHERE id = ?`, result.RunContextID).Scan(&manifestSHA, &manifestJSON); err != nil {
		t.Fatal(err)
	}
	target, err := store.LoadCurrentCanonicalReviewTarget(ctx, "connection-1", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if manifestSHA != target.ManifestSHA256 || string(manifestJSON) != string(target.ManifestJSON) {
		t.Fatalf("context evidence = %s %s", manifestSHA, manifestJSON)
	}
}

func TestPrepareReviewRunIsIdempotentForSameFacts(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	seedReviewProfileVersion(t, ctx, store, "profile-1", "profile-version-1")
	input := testPrepareReviewRunInput()

	first, err := store.PrepareReviewRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input.RequestedAt = input.RequestedAt.Add(time.Hour)
	second, err := store.PrepareReviewRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || first.IntentID != second.IntentID || first.RunID != second.RunID || first.RunContextID != second.RunContextID || first.IdempotencyKey != second.IdempotencyKey {
		t.Fatalf("first = %+v, second = %+v", first, second)
	}
	for _, table := range []string{"review_intents", "review_runs", "review_run_contexts"} {
		assertTableCount(t, ctx, store.db, table, 1)
	}
	assertTableCount(t, ctx, store.db, "review_run_events", 2)
}

func TestPrepareReviewRunRejectsConflictingIdempotencyFacts(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	seedReviewProfileVersion(t, ctx, store, "profile-1", "profile-version-1")
	input := testPrepareReviewRunInput()
	input.IdempotencyKey = "manual-command-1"
	if _, err := store.PrepareReviewRun(ctx, input); err != nil {
		t.Fatal(err)
	}
	input.EngineConfigJSON = []byte(`{"model":"different"}`)
	_, err := store.PrepareReviewRun(ctx, input)
	if !errors.Is(err, ErrReviewRunConflict) {
		t.Fatalf("error = %v", err)
	}
	assertTableCount(t, ctx, store.db, "review_intents", 1)
	assertTableCount(t, ctx, store.db, "review_runs", 1)
	assertTableCount(t, ctx, store.db, "review_run_contexts", 1)
}

func TestPrepareReviewRunRequiresProfileVersionAndPublicationDisabled(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	input := testPrepareReviewRunInput()
	if _, err := store.PrepareReviewRun(ctx, input); err == nil || !strings.Contains(err.Error(), "profile version") {
		t.Fatalf("missing profile error = %v", err)
	}
	seedReviewProfileVersion(t, ctx, store, "profile-1", "profile-version-1")
	if _, err := store.db.ExecContext(ctx, `UPDATE system_state SET value = 'enabled' WHERE key = 'publication_mode'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareReviewRun(ctx, input); err == nil || !strings.Contains(err.Error(), "publication mode disabled") {
		t.Fatalf("publication error = %v", err)
	}
	for _, table := range []string{"review_intents", "review_runs", "review_run_contexts", "review_run_events"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func TestPrepareReviewRunRequiresCurrentCanonicalTarget(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedReviewProfileVersion(t, ctx, store, "profile-1", "profile-version-1")
	_, err := store.PrepareReviewRun(ctx, testPrepareReviewRunInput())
	if !errors.Is(err, ErrCanonicalReviewTargetNotFound) {
		t.Fatalf("error = %v", err)
	}
	for _, table := range []string{"review_intents", "review_runs", "review_run_contexts", "review_run_events"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func testPrepareReviewRunInput() PrepareReviewRunInput {
	return PrepareReviewRunInput{
		ConnectionID: "connection-1", PullRequestID: "pr-1",
		ProfileID: "profile-1", ProfileVersionID: "profile-version-1",
		TriggerKind: "manual", TriggerSHA256: strings.Repeat("a", 64),
		UserContextSHA256: strings.Repeat("b", 64), EngineKind: "cli",
		EngineConfigJSON: []byte(" { \"model\" : \"test\" } "), AccessMode: "diff_only",
		RequestedAt: time.Unix(30, 0).UTC(),
	}
}

func seedReviewProfileVersion(t *testing.T, ctx context.Context, store *Store, profileID, versionID string) {
	t.Helper()
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO review_profiles(id, profile_key, created_at_us) VALUES (?, ?, 20)`, profileID, profileID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO review_profile_versions(
 id, profile_id, version, name, description, instructions, output_schema_version,
 settings_json, content_sha256, created_at_us)
VALUES (?, ?, 1, 'Default', '', 'Review carefully.', 1, '{}', ?, 20)`,
		versionID, profileID, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
}
