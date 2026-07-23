package sqlite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadAutomaticWatchRuleTargetReturnsCurrentFactsAndRules(t *testing.T) {
	ctx := context.Background()
	store, _ := seedPolicyRecordFixture(t, ctx)

	target, err := store.LoadAutomaticWatchRuleTarget(ctx, "connection-1", "pr-1")
	if err != nil {
		t.Fatal(err)
	}
	if target.ConnectionID != "connection-1" || target.PullRequestID != "pr-1" ||
		target.Canonical.ObservationID == "" || target.Canonical.RevisionID == "" ||
		target.Facts.RepositoryID != 1042 || target.Facts.RepositoryFullName != "owner/repo-1" ||
		target.Facts.AuthorLogin != "author" || target.Facts.IsDraft || target.Facts.State != "open" ||
		target.Facts.BaseRef != "main" || !reflect.DeepEqual(target.Facts.Labels, []string{}) ||
		!reflect.DeepEqual(target.Facts.Relationships, []string{}) {
		t.Fatalf("target facts = %+v", target)
	}
	if len(target.Rules) != 1 {
		t.Fatalf("rules = %+v", target.Rules)
	}
	rule := target.Rules[0]
	if rule.PolicySetID != "policy-set-1" || rule.RuleID != "rule-1" || rule.VersionID != "rule-version-1" ||
		rule.RuleKey != "assigned-default" || rule.Priority != 0 || rule.TriggerKind != "automatic" ||
		rule.ProfileID != "profile-1" || rule.ProfileVersionID != "profile-version-1" ||
		string(rule.MatchJSON) != "{}" || string(rule.ReviewJSON) != "{}" {
		t.Fatalf("rule = %+v", rule)
	}
}

func TestListAutomaticWatchRuleTargetIDsExcludesTerminalPullRequests(t *testing.T) {
	ctx := context.Background()
	store, attached := seedCurrentCanonicalReviewTarget(t, ctx)
	ids, err := store.ListAutomaticWatchRuleTargetIDs(ctx, "connection-1")
	if err != nil || !reflect.DeepEqual(ids, []string{"pr-1"}) {
		t.Fatalf("open target IDs = %v, err=%v", ids, err)
	}
	var nextObservationAt, nextProjectionAt int64
	if err := store.db.QueryRowContext(ctx, `
SELECT observation.github_updated_at_us + 1, projection.updated_at_us + 1
FROM pull_request_projection_state AS projection
JOIN pull_request_observations AS observation ON observation.id = projection.current_observation_id
WHERE projection.pull_request_id = 'pr-1'`).Scan(&nextObservationAt, &nextProjectionAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO pull_request_observations(
 id, connection_id, repository_id, pull_request_id, revision_id, head_sha, base_sha,
 source_kind, source_priority, facts_format_version, facts_sha256, title,
 author_login, author_database_id, body_sha256, labels_json, is_draft, base_ref,
 requested_reviewers_json, relationship_set_json, github_state,
 github_updated_at_us, observed_at_us, created_at_us)
VALUES ('terminal-watch-observation', 'connection-1', 'repo-1', 'pr-1', ?, ?, ?,
 'direct_refresh', 30, 1, ?, 'Merged pull request', 'author', 8001, ?, '[]', 0, 'main', '[]', '[]', 'merged',
 ?, ?, ?)`, attached.RevisionID, projectionHeadSHA, projectionBaseSHA, strings.Repeat("f", 64), projectionDigest, nextObservationAt, nextObservationAt, nextObservationAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
UPDATE pull_request_projection_state
SET current_observation_id = 'terminal-watch-observation', updated_at_us = ?
WHERE pull_request_id = 'pr-1'`, nextProjectionAt); err != nil {
		t.Fatal(err)
	}
	revision := testCanonicalRevision(t)
	if _, err := store.AttachCanonicalRevision(ctx, CanonicalRevisionInput{
		ConnectionID: "connection-1", ObservationID: "terminal-watch-observation",
		HeadSHA: projectionHeadSHA, BaseSHA: projectionBaseSHA, IdentityKey: revision.IdentityKey,
		ManifestSHA256: revision.ManifestSHA256, ManifestJSON: revision.Manifest, EntryCount: 1,
		AttachedAt: time.Unix(100, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	ids, err = store.ListAutomaticWatchRuleTargetIDs(ctx, "connection-1")
	if err != nil || len(ids) != 0 {
		t.Fatalf("terminal target IDs = %v, err=%v", ids, err)
	}
}

func TestLoadAutomaticWatchRuleTargetRejectsStaleOrInvalidStoredRules(t *testing.T) {
	ctx := context.Background()
	store, _ := seedCurrentCanonicalReviewTarget(t, ctx)
	profile := mustCreatePolicyTestProfile(t, ctx, store)

	if _, err := store.CreatePolicySetGeneration(ctx, PolicySetGenerationInput{
		Generation: 1,
		Rules: []WatchRuleVersionInput{{
			RuleKey: "assigned-default", Enabled: true, Priority: 0,
			TriggerKind: "automatic", ExternalActionPolicy: "require_confirmation",
			ProfileID: profile.ProfileID, ProfileVersionID: profile.VersionID,
			MatchJSON: []byte(`{"unknown":true}`), ReviewJSON: []byte(`{}`), PublicationJSON: []byte(`{}`),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadAutomaticWatchRuleTarget(ctx, "connection-1", "pr-1"); err == nil {
		t.Fatal("invalid watch rule match accepted")
	}

	store, _ = seedCurrentCanonicalReviewTarget(t, ctx)
	if _, err := store.db.ExecContext(ctx, `UPDATE pull_request_projection_state SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadAutomaticWatchRuleTarget(ctx, "connection-1", "pr-1"); !errors.Is(err, ErrAutomaticWatchRuleTargetNotFound) {
		t.Fatalf("stale target error = %v", err)
	}
}

func TestDecodeAutomaticWatchRuleFactSetRejectsMalformedStoredFacts(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`[1]`), []byte(`null`), []byte(`["x"] trailing`)} {
		if _, err := decodeAutomaticWatchRuleFactSet(raw); err == nil {
			t.Fatalf("invalid fact set accepted: %q", raw)
		}
	}
}
