package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLoadReviewRunExecutionTargetReturnsOnlyVerifiedPreparedFacts(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	profile := createExecutionTargetProfile(t, ctx, store)
	input := testPrepareReviewRunInput()
	input.ProfileID, input.ProfileVersionID = profile.ProfileID, profile.VersionID
	prepared, err := store.PrepareReviewRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}

	target, err := store.LoadReviewRunExecutionTarget(ctx, prepared.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if target.RunID != prepared.RunID || target.IntentID != prepared.IntentID || target.RunContextID != prepared.RunContextID ||
		target.Owner != "owner" || target.Repository != "repo-1" || target.Number != 42 ||
		target.Canonical.RevisionID == "" || len(target.Canonical.ManifestJSON) == 0 ||
		target.Profile.ProfileID != profile.ProfileID || target.Profile.ProfileVersionID != profile.VersionID ||
		target.Profile.Name != "Default" || target.Profile.Instructions != "Review carefully." || string(target.Profile.SettingsJSON) != "{}" {
		t.Fatalf("target = %+v", target)
	}
	for _, table := range []string{"jobs", "domain_events", "outbox", "assessments"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
	assertTableCount(t, ctx, store.db, "review_run_events", 2)
}

func TestLoadReviewRunExecutionTargetFailsClosedForStaleOrCompletedRun(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	profile := createExecutionTargetProfile(t, ctx, store)
	input := testPrepareReviewRunInput()
	input.ProfileID, input.ProfileVersionID = profile.ProfileID, profile.VersionID
	prepared, err := store.PrepareReviewRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO review_run_events(id, run_id, sequence, event_kind, payload_json, occurred_at_us, created_at_us)
VALUES ('event-running', ?, 3, 'running', '{}', 31, 31)`, prepared.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadReviewRunExecutionTarget(ctx, prepared.RunID); err == nil || !strings.Contains(err.Error(), "no longer preparing") {
		t.Fatalf("running target error = %v", err)
	}
}

func createExecutionTargetProfile(t *testing.T, ctx context.Context, store *Store) CreateReviewProfileVersionResult {
	t.Helper()
	profile, err := store.CreateReviewProfileVersion(ctx, CreateReviewProfileVersionInput{
		ProfileKey: "default", Version: 1, Name: "Default", Instructions: "Review carefully.", SettingsJSON: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestLoadReviewRunExecutionTargetRejectsMissingRun(t *testing.T) {
	store := openMigratedStore(t, context.Background())
	_, err := store.LoadReviewRunExecutionTarget(context.Background(), "missing")
	if !errors.Is(err, ErrReviewRunExecutionTargetNotFound) {
		t.Fatalf("error = %v", err)
	}
}
