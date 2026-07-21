package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCreatePolicySetGenerationCreatesNormalizedRuleVersions(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	profile := mustCreatePolicyTestProfile(t, ctx, store)

	result, err := store.CreatePolicySetGeneration(ctx, PolicySetGenerationInput{
		Generation: 1,
		Rules: []WatchRuleVersionInput{
			{
				RuleKey:              " Assigned-Default ",
				Enabled:              true,
				Priority:             10,
				TriggerKind:          "automatic",
				ExternalActionPolicy: "require_confirmation",
				ProfileID:            profile.ProfileID,
				ProfileVersionID:     profile.VersionID,
				MatchJSON:            []byte(` { "repositories" : ["owner/repo"], "author" : "bot" } `),
				ReviewJSON:           []byte(` { "access_mode" : "diff_only" } `),
				PublicationJSON:      []byte(` { "mode" : "advisory" } `),
			},
			{
				RuleKey:              "ignore-drafts",
				Enabled:              false,
				Priority:             0,
				TriggerKind:          "ignore",
				ExternalActionPolicy: "advisory_only",
				MatchJSON:            []byte(`{"draft":true}`),
				ReviewJSON:           []byte(`{}`),
				PublicationJSON:      []byte(`{}`),
			},
		},
		CreatedAt: time.Unix(20, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.PolicySetID == "" || result.Generation != 1 || len(result.ContentSHA256) != 64 || len(result.RuleVersions) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if result.RuleVersions[0].RuleKey != "ignore-drafts" || result.RuleVersions[1].RuleKey != "assigned-default" {
		t.Fatalf("rule results not priority ordered: %+v", result.RuleVersions)
	}

	var match, review, publication string
	if err := store.db.QueryRowContext(ctx, `
SELECT match_json, review_json, publication_json
FROM watch_rule_versions WHERE id = ?`, result.RuleVersions[1].VersionID).Scan(&match, &review, &publication); err != nil {
		t.Fatal(err)
	}
	if match != `{"author":"bot","repositories":["owner/repo"]}` || review != `{"access_mode":"diff_only"}` || publication != `{"mode":"advisory"}` {
		t.Fatalf("normalized rule JSON = %q %q %q", match, review, publication)
	}
	var enabled int
	var currentVersionID string
	if err := store.db.QueryRowContext(ctx, `SELECT enabled, current_version_id FROM watch_rules WHERE rule_key = 'assigned-default'`).Scan(&enabled, &currentVersionID); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 || currentVersionID != result.RuleVersions[1].VersionID {
		t.Fatalf("watch rule pointer = enabled:%d version:%q", enabled, currentVersionID)
	}
	for _, table := range []string{"jobs", "domain_events", "outbox", "policy_evaluations", "proposals", "decisions", "publication_effects"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func TestCreatePolicySetGenerationIsIdempotentAndAdvancesRulePointers(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	profile := mustCreatePolicyTestProfile(t, ctx, store)
	input := testPolicySetGenerationInput(profile)

	first, err := store.CreatePolicySetGeneration(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input.CreatedAt = input.CreatedAt.Add(time.Hour)
	second, err := store.CreatePolicySetGeneration(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || first.PolicySetID != second.PolicySetID || first.ContentSHA256 != second.ContentSHA256 || first.RuleVersions[0] != second.RuleVersions[0] {
		t.Fatalf("idempotence results = %+v %+v", first, second)
	}

	input.Generation = 2
	input.Rules[0].Enabled = false
	input.Rules[0].Priority = 2
	input.Rules[0].ReviewJSON = []byte(`{"access_mode":"selected_files"}`)
	third, err := store.CreatePolicySetGeneration(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !third.Created || third.PolicySetID == first.PolicySetID || third.RuleVersions[0].VersionID == first.RuleVersions[0].VersionID || third.RuleVersions[0].Version != 2 {
		t.Fatalf("advanced result = %+v", third)
	}
	var enabled int
	var currentVersion string
	if err := store.db.QueryRowContext(ctx, `SELECT enabled, current_version_id FROM watch_rules WHERE id = ?`, third.RuleVersions[0].RuleID).Scan(&enabled, &currentVersion); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || currentVersion != third.RuleVersions[0].VersionID {
		t.Fatalf("current pointer = enabled:%d version:%q", enabled, currentVersion)
	}
	assertTableCount(t, ctx, store.db, "policy_sets", 2)
	assertTableCount(t, ctx, store.db, "watch_rules", 1)
	assertTableCount(t, ctx, store.db, "watch_rule_versions", 2)
}

func TestCreatePolicySetGenerationRejectsConflictsAndInvalidRules(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	profile := mustCreatePolicyTestProfile(t, ctx, store)
	input := testPolicySetGenerationInput(profile)
	if _, err := store.CreatePolicySetGeneration(ctx, input); err != nil {
		t.Fatal(err)
	}

	changed := input
	changed.Rules = append([]WatchRuleVersionInput(nil), input.Rules...)
	changed.Rules[0].Priority = 9
	if _, err := store.CreatePolicySetGeneration(ctx, changed); !errors.Is(err, ErrPolicySetGenerationConflict) {
		t.Fatalf("changed generation error = %v", err)
	}

	for _, invalid := range []PolicySetGenerationInput{
		{},
		{Generation: 2, Rules: []WatchRuleVersionInput{input.Rules[0], input.Rules[0]}},
		{Generation: 2, Rules: []WatchRuleVersionInput{{RuleKey: "bad", Priority: 0, TriggerKind: "automatic", ExternalActionPolicy: "advisory_only", MatchJSON: []byte(`{}`), ReviewJSON: []byte(`{}`), PublicationJSON: []byte(`{}`)}}},
		{Generation: 2, Rules: []WatchRuleVersionInput{{RuleKey: "bad", Priority: 0, TriggerKind: "ignore", ExternalActionPolicy: "advisory_only", ProfileID: profile.ProfileID, ProfileVersionID: profile.VersionID, MatchJSON: []byte(`{}`), ReviewJSON: []byte(`{}`), PublicationJSON: []byte(`{}`)}}},
		{Generation: 2, Rules: []WatchRuleVersionInput{{RuleKey: "bad", Priority: 0, TriggerKind: "ignore", ExternalActionPolicy: "advisory_only", MatchJSON: []byte(`{"x":1,"x":2}`), ReviewJSON: []byte(`{}`), PublicationJSON: []byte(`{}`)}}},
	} {
		if _, err := store.CreatePolicySetGeneration(ctx, invalid); err == nil {
			t.Fatalf("invalid input accepted: %+v", invalid)
		}
	}
	assertTableCount(t, ctx, store.db, "policy_sets", 1)
	assertTableCount(t, ctx, store.db, "watch_rule_versions", 1)
}

func mustCreatePolicyTestProfile(t *testing.T, ctx context.Context, store *Store) CreateReviewProfileVersionResult {
	t.Helper()
	profile, err := store.CreateReviewProfileVersion(ctx, testReviewProfileVersionInput())
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func testPolicySetGenerationInput(profile CreateReviewProfileVersionResult) PolicySetGenerationInput {
	return PolicySetGenerationInput{
		Generation: 1,
		Rules: []WatchRuleVersionInput{{
			RuleKey:              "assigned-default",
			Enabled:              true,
			Priority:             1,
			TriggerKind:          "automatic",
			ExternalActionPolicy: "require_confirmation",
			ProfileID:            profile.ProfileID,
			ProfileVersionID:     profile.VersionID,
			MatchJSON:            []byte(`{"repository":"owner/repo"}`),
			ReviewJSON:           []byte(`{"access_mode":"diff_only"}`),
			PublicationJSON:      []byte(`{"mode":"advisory"}`),
		}},
		CreatedAt: time.Unix(30, 0).UTC(),
	}
}
