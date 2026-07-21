package sqlite

import (
	"context"
	"strings"
	"testing"
)

func TestReviewExecutionLedgerAnchorsImmutableRecordsToCanonicalEvidence(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedMetadataObservation(t, ctx, store.db, "pr-1", "observation-1", projectionHeadSHA, projectionBaseSHA, 10)
	seedCanonicalRevisionManifest(t, ctx, store.db, "pr-1", "revision-1", "manifest-1", projectionHeadSHA, projectionBaseSHA, 11)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO observation_revision_links(
 id, observation_id, pull_request_id, connection_id, revision_id, manifest_id,
 attached_at_us, created_at_us)
VALUES ('link-1', 'observation-1', 'pr-1', 'connection-1', 'revision-1', 'manifest-1', 12, 12)`); err != nil {
		t.Fatal(err)
	}

	for _, statement := range []string{
		`INSERT INTO review_profiles(id, profile_key, created_at_us)
VALUES ('profile-1', 'default', 13)`,
		`INSERT INTO review_profile_versions(
 id, profile_id, version, name, description, instructions, output_schema_version,
 settings_json, content_sha256, created_at_us)
VALUES ('profile-version-1', 'profile-1', 1, 'Default', '', 'Review carefully.', 1,
 '{}', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 13)`,
		`INSERT INTO review_intents(
 id, connection_id, repository_id, pull_request_id, revision_id, observation_id,
 profile_id, profile_version_id, trigger_kind, idempotency_key, trigger_sha256,
 user_context_sha256, correlation_id, requested_at_us, created_at_us)
VALUES ('intent-1', 'connection-1', 'repo-1', 'pr-1', 'revision-1', 'observation-1',
 'profile-1', 'profile-version-1', 'manual', 'intent:manual:1',
 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', NULL, 'correlation-1', 14, 14)`,
		`INSERT INTO review_runs(
 id, intent_id, connection_id, pull_request_id, revision_id, observation_id,
 attempt_number, engine_kind, engine_config_json, started_at_us, created_at_us)
VALUES ('run-1', 'intent-1', 'connection-1', 'pr-1', 'revision-1', 'observation-1',
 1, 'cli', '{}', 15, 15)`,
		`INSERT INTO review_run_contexts(
 id, run_id, intent_id, pull_request_id, revision_id, observation_id,
 context_format_version, access_mode, manifest_sha256, manifest_json, created_at_us)
VALUES ('context-1', 'run-1', 'intent-1', 'pr-1', 'revision-1', 'observation-1',
 1, 'diff_only', 'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc', '{}', 16)`,
		`INSERT INTO assessments(
 id, run_id, intent_id, pull_request_id, revision_id, observation_id,
 schema_version, verdict, summary, confidence, limitations_json, coverage_json,
 output_sha256, created_at_us)
VALUES ('assessment-1', 'run-1', 'intent-1', 'pr-1', 'revision-1', 'observation-1',
 1, 'concerns', 'Needs a guard.', 'high', '[]',
 '{"status":"complete","changed_files_total":1,"reviewed_files":1,"omitted":[]}',
 'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd', 17)`,
		`INSERT INTO findings(
 id, assessment_id, run_id, pull_request_id, revision_id, observation_id,
 client_id, fingerprint_sha256, severity, category, path, line, side, message,
 evidence, suggestion, anchor_status, created_at_us)
VALUES ('finding-1', 'assessment-1', 'run-1', 'pr-1', 'revision-1', 'observation-1',
 'one', 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee',
 'high', 'correctness', 'internal/example.go', 42, 'RIGHT', 'Guard nil.',
 'The path can be nil.', 'Check before use.', 'valid', 18)`,
		`INSERT INTO validation_warnings(
 id, assessment_id, run_id, pull_request_id, revision_id, observation_id,
 warning_code, message, details_json, created_at_us)
VALUES ('warning-1', 'assessment-1', 'run-1', 'pr-1', 'revision-1', 'observation-1',
 'coverage_adjusted', 'Coverage normalized.', '{}', 19)`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("insert immutable review ledger record: %v\n%s", err, statement)
		}
	}

	for _, table := range []string{
		"review_profiles", "review_profile_versions", "review_intents", "review_runs",
		"review_run_contexts", "assessments", "findings", "validation_warnings",
	} {
		if _, err := store.db.ExecContext(ctx, "DELETE FROM "+table); err == nil {
			t.Fatalf("deleting immutable %s was accepted", table)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE assessments SET summary = 'changed' WHERE id = 'assessment-1'`); err == nil {
		t.Fatal("updating immutable assessment was accepted")
	}
}

func TestReviewExecutionLedgerRejectsNonCanonicalAndMismatchedEvidence(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	seedProjectionConnection(t, ctx, store.db)
	seedProjectionPullRequest(t, ctx, store.db, "repo-1", "pr-1", 42)
	seedMetadataObservation(t, ctx, store.db, "pr-1", "observation-1", projectionHeadSHA, projectionBaseSHA, 10)
	seedCanonicalRevisionManifest(t, ctx, store.db, "pr-1", "revision-1", "manifest-1", projectionHeadSHA, projectionBaseSHA, 11)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO review_profiles(id, profile_key, created_at_us) VALUES ('profile-1', 'default', 12);
INSERT INTO review_profile_versions(
 id, profile_id, version, name, description, instructions, output_schema_version,
 settings_json, content_sha256, created_at_us)
VALUES ('profile-version-1', 'profile-1', 1, 'Default', '', 'Review carefully.', 1,
 '{}', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 12)`); err != nil {
		t.Fatal(err)
	}

	_, err := store.db.ExecContext(ctx, `
INSERT INTO review_intents(
 id, connection_id, repository_id, pull_request_id, revision_id, observation_id,
 profile_id, profile_version_id, trigger_kind, idempotency_key, trigger_sha256,
 requested_at_us, created_at_us)
VALUES ('intent-1', 'connection-1', 'repo-1', 'pr-1', 'revision-1', 'observation-1',
 'profile-1', 'profile-version-1', 'manual', 'intent:manual:1',
 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 13, 13)`)
	if err == nil {
		t.Fatal("intent accepted canonical revision without its observation attachment")
	}

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO observation_revision_links(
 id, observation_id, pull_request_id, connection_id, revision_id, manifest_id,
 attached_at_us, created_at_us)
VALUES ('link-1', 'observation-1', 'pr-1', 'connection-1', 'revision-1', 'manifest-1', 12, 12)`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO review_intents(
 id, connection_id, repository_id, pull_request_id, revision_id, observation_id,
 profile_id, profile_version_id, trigger_kind, idempotency_key, trigger_sha256,
 requested_at_us, created_at_us)
VALUES ('intent-1', 'connection-1', 'repo-1', 'pr-1', 'revision-1', 'observation-1',
 'profile-1', 'profile-version-1', 'manual', 'intent:manual:1',
 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 13, 13)`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO review_runs(
 id, intent_id, connection_id, pull_request_id, revision_id, observation_id,
 attempt_number, engine_kind, engine_config_json, started_at_us, created_at_us)
VALUES ('run-1', 'intent-1', 'connection-1', 'pr-1', 'revision-1', 'observation-1',
 1, 'cli', '{}', 14, 14)`); err != nil {
		t.Fatal(err)
	}
	_, err = store.db.ExecContext(ctx, `
INSERT INTO assessments(
 id, run_id, intent_id, pull_request_id, revision_id, observation_id,
 schema_version, verdict, summary, confidence, limitations_json, coverage_json,
 output_sha256, created_at_us)
VALUES ('assessment-bad', 'run-1', 'intent-1', 'pr-1', 'revision-1', 'observation-1',
 1, 'pass', 'ok', 'high', '[]', '{}',
 'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd', 15)`)
	if err == nil || !strings.Contains(err.Error(), "coverage") {
		t.Fatalf("assessment accepted invalid coverage contract: %v", err)
	}
}
