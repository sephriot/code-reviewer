package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCreateReviewProfileVersionCreatesNormalizedImmutableVersion(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)

	result, err := store.CreateReviewProfileVersion(ctx, CreateReviewProfileVersionInput{
		ProfileKey:   " Default ",
		Version:      1,
		Name:         "  Default review  ",
		Description:  "  Reviews pull requests.  ",
		Instructions: "\r\n  Review carefully.\r\n",
		SettingsJSON: []byte(` { "timeout_seconds" : 30, "model" : "test" } `),
		CreatedAt:    time.Unix(20, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.ProfileID == "" || result.VersionID == "" || result.Version != 1 || result.ProfileKey != "default" || len(result.ContentSHA256) != 64 {
		t.Fatalf("result = %+v", result)
	}
	if result.ProfileID != stableID("review-profile", "default") || result.VersionID != stableID("review-profile-version", result.ProfileID, "1") {
		t.Fatalf("result IDs = %+v", result)
	}

	var key, name, description, instructions, settings, digest string
	if err := store.db.QueryRowContext(ctx, `
SELECT profile.profile_key, version.name, version.description, version.instructions,
       version.settings_json, version.content_sha256
FROM review_profiles AS profile
JOIN review_profile_versions AS version ON version.profile_id = profile.id
WHERE version.id = ?`, result.VersionID).Scan(&key, &name, &description, &instructions, &settings, &digest); err != nil {
		t.Fatal(err)
	}
	if key != "default" || name != "Default review" || description != "Reviews pull requests." || instructions != "Review carefully." || settings != `{"model":"test","timeout_seconds":30}` || digest != result.ContentSHA256 {
		t.Fatalf("stored = key=%q name=%q description=%q instructions=%q settings=%q digest=%q", key, name, description, instructions, settings, digest)
	}
	assertTableCount(t, ctx, store.db, "review_profiles", 1)
	assertTableCount(t, ctx, store.db, "review_profile_versions", 1)
	for _, table := range []string{"jobs", "domain_events", "outbox"} {
		assertTableCount(t, ctx, store.db, table, 0)
	}
}

func TestCreateReviewProfileVersionIsIdempotentAndAddsNewVersions(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	input := testReviewProfileVersionInput()

	first, err := store.CreateReviewProfileVersion(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input.CreatedAt = input.CreatedAt.Add(time.Hour)
	second, err := store.CreateReviewProfileVersion(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created ||
		first.ProfileID != second.ProfileID || first.VersionID != second.VersionID ||
		first.ProfileKey != second.ProfileKey || first.Version != second.Version ||
		first.ContentSHA256 != second.ContentSHA256 {
		t.Fatalf("first = %+v, second = %+v", first, second)
	}

	input.Version = 2
	input.Instructions = "Review security and correctness."
	third, err := store.CreateReviewProfileVersion(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !third.Created || third.ProfileID != first.ProfileID || third.VersionID == first.VersionID || third.Version != 2 {
		t.Fatalf("third = %+v", third)
	}
	assertTableCount(t, ctx, store.db, "review_profiles", 1)
	assertTableCount(t, ctx, store.db, "review_profile_versions", 2)
}

func TestCreateReviewProfileVersionRejectsChangedExistingVersion(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	input := testReviewProfileVersionInput()
	if _, err := store.CreateReviewProfileVersion(ctx, input); err != nil {
		t.Fatal(err)
	}
	input.Name = "Changed"
	_, err := store.CreateReviewProfileVersion(ctx, input)
	if !errors.Is(err, ErrReviewProfileVersionConflict) {
		t.Fatalf("error = %v", err)
	}
	assertTableCount(t, ctx, store.db, "review_profiles", 1)
	assertTableCount(t, ctx, store.db, "review_profile_versions", 1)
}

func TestCreateReviewProfileVersionRejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	store := openMigratedStore(t, ctx)
	cases := []CreateReviewProfileVersionInput{
		{},
		{ProfileKey: "default", Version: 0, Name: "Default", Instructions: "Review", SettingsJSON: []byte(`{}`)},
		{ProfileKey: "default", Version: 1, Name: "", Instructions: "Review", SettingsJSON: []byte(`{}`)},
		{ProfileKey: "default", Version: 1, Name: "Default", Instructions: "", SettingsJSON: []byte(`{}`)},
		{ProfileKey: "default", Version: 1, Name: "Default", Instructions: "Review", SettingsJSON: []byte(`[]`)},
		{ProfileKey: "default", Version: 1, Name: "Default", Instructions: "Review", SettingsJSON: []byte(`{"x":1} trailing`)},
		{ProfileKey: strings.Repeat("a", maxReviewProfileKeyBytes+1), Version: 1, Name: "Default", Instructions: "Review", SettingsJSON: []byte(`{}`)},
	}
	for _, input := range cases {
		if _, err := store.CreateReviewProfileVersion(ctx, input); err == nil {
			t.Fatalf("input %+v was accepted", input)
		}
	}
	assertTableCount(t, ctx, store.db, "review_profiles", 0)
	assertTableCount(t, ctx, store.db, "review_profile_versions", 0)
}

func testReviewProfileVersionInput() CreateReviewProfileVersionInput {
	return CreateReviewProfileVersionInput{
		ProfileKey: "default", Version: 1, Name: "Default", Description: "",
		Instructions: "Review carefully.", SettingsJSON: []byte(`{"model":"test"}`),
		CreatedAt: time.Unix(30, 0).UTC(),
	}
}
