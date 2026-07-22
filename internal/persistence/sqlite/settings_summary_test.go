package sqlite

import (
	"context"
	"testing"
)

func TestSettingsSummaryCountsConfiguredProfilesAndActiveRulesWithoutWrites(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	profile := mustCreatePolicyTestProfile(t, ctx, store)
	if _, err := store.CreateReviewProfileVersion(ctx, CreateReviewProfileVersionInput{
		ProfileKey: "second", Version: 1, Name: "Second", Instructions: "Review carefully.", SettingsJSON: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreatePolicySetGeneration(ctx, PolicySetGenerationInput{
		Generation: 1,
		Rules: []WatchRuleVersionInput{
			{
				RuleKey: "active", Enabled: true, Priority: 1, TriggerKind: "automatic", ExternalActionPolicy: "require_confirmation",
				ProfileID: profile.ProfileID, ProfileVersionID: profile.VersionID,
				MatchJSON: []byte(`{}`), ReviewJSON: []byte(`{}`), PublicationJSON: []byte(`{}`),
			},
			{
				RuleKey: "disabled", Enabled: false, Priority: 2, TriggerKind: "ignore", ExternalActionPolicy: "advisory_only",
				MatchJSON: []byte(`{}`), ReviewJSON: []byte(`{}`), PublicationJSON: []byte(`{}`),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	before := map[string]int{}
	for _, table := range []string{"system_state", "review_profiles", "review_profile_versions", "watch_rules", "watch_rule_versions", "jobs", "domain_events", "outbox"} {
		var count int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		before[table] = count
	}
	summary, err := store.SettingsSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.PublicationMode != PublicationModeDisabled || summary.ActiveWatchRules != 1 || summary.ConfiguredProfiles != 2 {
		t.Fatalf("summary = %+v", summary)
	}
	for table, want := range before {
		var got int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s count = %d, want %d", table, got, want)
		}
	}
}
