package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sephriot/code-reviewer/internal/application/assessment"
	"github.com/sephriot/code-reviewer/internal/application/canonical"
	"github.com/sephriot/code-reviewer/internal/application/publishworker"
	storagesqlite "github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

func TestDatabaseMigrateRequiresExplicitApply(t *testing.T) {
	var output bytes.Buffer
	err := run(
		context.Background(),
		[]string{"db", "migrate", "--database", filepath.Join(t.TempDir(), "control-plane.db")},
		&output,
		&output,
	)
	if err == nil || !strings.Contains(err.Error(), "--apply") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestDatabaseStatusDoesNotCreateMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	var output bytes.Buffer
	err := run(context.Background(), []string{"db", "status", "--database", path}, &output, &output)
	if err == nil {
		t.Fatal("status accepted a missing database")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("status created database: %v", statErr)
	}
}

func TestDatabaseMigrateThenStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control-plane.db")
	var output bytes.Buffer
	if err := run(context.Background(), []string{"db", "migrate", "--database", path, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"current": 10`) {
		t.Fatalf("migration output = %s", output.String())
	}
	output.Reset()
	if err := run(context.Background(), []string{"db", "status", "--database", path}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"pending": 0`) {
		t.Fatalf("status output = %s", output.String())
	}
}

func TestDatabaseMigrateUsesEnvironmentThenCLIOverride(t *testing.T) {
	environmentPath := filepath.Join(t.TempDir(), "environment.db")
	overridePath := filepath.Join(t.TempDir(), "override.db")
	t.Setenv("REVIEWD_DATABASE_PATH", environmentPath)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"db", "migrate", "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(environmentPath); err != nil {
		t.Fatalf("environment database was not created: %v", err)
	}
	output.Reset()
	if err := run(context.Background(), []string{"db", "migrate", "--database", overridePath, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(overridePath); err != nil {
		t.Fatalf("CLI override database was not created: %v", err)
	}
}

func TestProfileCreateStoresImmutableVersionFromBoundedFiles(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "control-plane.db")
	var output bytes.Buffer
	if err := run(context.Background(), []string{"db", "migrate", "--database", databasePath, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	instructions := filepath.Join(directory, "instructions.txt")
	settings := filepath.Join(directory, "settings.json")
	if err := os.WriteFile(instructions, []byte("Review carefully."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"model":"test","timeout_seconds":30}`), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	err := run(context.Background(), []string{
		"profile", "create", "--database", databasePath, "--key", "default", "--version", "1", "--name", "Default",
		"--instructions-file", instructions, "--settings-file", settings,
	}, &output, &output)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		ProfileKey string `json:"ProfileKey"`
		Version    int    `json:"Version"`
		Created    bool   `json:"Created"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ProfileKey != "default" || result.Version != 1 || !result.Created || strings.Contains(output.String(), "Review carefully") {
		t.Fatalf("profile create output = %s", output.String())
	}
}

func TestManualControlRejectsSecretBearingInput(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"profile", "create", "--token=never-store"}, &output, &output); err == nil || !strings.Contains(err.Error(), "cannot carry secrets") {
		t.Fatalf("secret flag error = %v", err)
	}
	databasePath := filepath.Join(t.TempDir(), "control-plane.db")
	if err := run(context.Background(), []string{"db", "migrate", "--database", databasePath, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	instructions := filepath.Join(directory, "instructions.txt")
	settings := filepath.Join(directory, "settings.json")
	if err := os.WriteFile(instructions, []byte("Review carefully."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"api_token":"never-store"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{
		"profile", "create", "--database", databasePath, "--key", "default", "--version", "1", "--name", "Default",
		"--instructions-file", instructions, "--settings-file", settings,
	}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "secret-bearing") || strings.Contains(err.Error(), "never-store") {
		t.Fatalf("secret settings error = %v", err)
	}
}

func TestPolicyApplyCreatesImmutablePolicySetFromStrictFile(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "control-plane.db")
	var output bytes.Buffer
	if err := run(context.Background(), []string{"db", "migrate", "--database", databasePath, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	rulesFile := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(rulesFile, []byte(`{"rules":[{"key":"observe-all","enabled":true,"priority":0,"trigger_kind":"track_only","external_action_policy":"advisory_only","match":{},"review":{},"publication":{}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(context.Background(), []string{"policy", "apply", "--database", databasePath, "--generation", "1", "--rules-file", rulesFile}, &output, &output); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Generation   int  `json:"Generation"`
		Created      bool `json:"Created"`
		RuleVersions []struct {
			RuleKey string `json:"RuleKey"`
		} `json:"RuleVersions"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Generation != 1 || !result.Created || len(result.RuleVersions) != 1 || result.RuleVersions[0].RuleKey != "observe-all" {
		t.Fatalf("policy apply output = %s", output.String())
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	assertCLIQueryCount(t, database, "policy_sets", 1)
	assertCLIQueryCount(t, database, "watch_rule_versions", 1)

	if err := os.WriteFile(rulesFile, []byte(`{"rules":[],"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	err = run(context.Background(), []string{"policy", "apply", "--database", databasePath, "--generation", "2", "--rules-file", rulesFile}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "strict JSON") {
		t.Fatalf("unknown field error = %v", err)
	}
	assertCLIQueryCount(t, database, "policy_sets", 1)
}

func TestManualReviewQueueNeverAcceptsPerRunEngineArguments(t *testing.T) {
	var output bytes.Buffer
	err := run(context.Background(), []string{
		"review", "queue", "--engine-argv-file", "/tmp/engine.json",
	}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("per-run engine argv error = %v", err)
	}
}

func TestPolicyEvaluateRecordsCurrentAssessmentWithoutPublication(t *testing.T) {
	databasePath, assessmentID, ruleVersionID := seedPolicyEvaluationCandidate(t)
	var output bytes.Buffer
	err := run(context.Background(), []string{
		"policy", "evaluate", "--database", databasePath,
		"--assessment-id", assessmentID, "--rule-key", "assigned-default", "--rule-version-id", ruleVersionID,
	}, &output, &output)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Outcome struct {
			Disposition string
		}
		Evaluation struct {
			PolicyEvaluationID string
			ProposalID         string
			Created            bool
		}
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Disposition != "propose_changes" || result.Evaluation.PolicyEvaluationID == "" || result.Evaluation.ProposalID == "" || !result.Evaluation.Created {
		t.Fatalf("policy evaluate output = %s", output.String())
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	assertCLIQueryCount(t, database, "policy_evaluations", 1)
	assertCLIQueryCount(t, database, "proposals", 1)
	assertCLIQueryCount(t, database, "proposal_revisions", 1)
	for _, table := range []string{"decisions", "publication_effects", "publication_attempts", "jobs", "domain_events", "outbox"} {
		assertCLIQueryCount(t, database, table, 0)
	}
}

func TestReviewScheduleSelectsRuleWithoutQueueingManualTrigger(t *testing.T) {
	databasePath, _, _ := seedPolicyEvaluationCandidate(t)
	var output bytes.Buffer
	err := run(context.Background(), []string{
		"review", "schedule", "--database", databasePath,
		"--connection-id", "connection-1", "--owner", "owner", "--repository", "repo", "--number", "42",
	}, &output, &output)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Matched     bool   `json:"Matched"`
		TriggerKind string `json:"TriggerKind"`
		Queued      any    `json:"Queued"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Matched || result.TriggerKind != "manual" || result.Queued != nil {
		t.Fatalf("review schedule output = %s", output.String())
	}
}

func TestPolicyEvaluateRejectsStaleAssessmentWithoutRecording(t *testing.T) {
	databasePath, assessmentID, ruleVersionID := seedPolicyEvaluationCandidate(t)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.Exec(`UPDATE pull_request_projection_state SET current_revision_id = NULL WHERE pull_request_id = 'pr-1'`); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = run(context.Background(), []string{
		"policy", "evaluate", "--database", databasePath,
		"--assessment-id", assessmentID, "--rule-key", "assigned-default", "--rule-version-id", ruleVersionID,
	}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "policy evaluation target") {
		t.Fatalf("stale assessment error = %v", err)
	}
	for _, table := range []string{"policy_evaluations", "proposals", "decisions", "publication_effects", "publication_attempts", "jobs", "domain_events", "outbox"} {
		assertCLIQueryCount(t, database, table, 0)
	}
}

func TestPolicyEvaluateRequiresInputsAndCurrentSchema(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"policy", "evaluate"}, &output, &output); err == nil || !strings.Contains(err.Error(), "--assessment-id") {
		t.Fatalf("missing arguments error = %v", err)
	}
	if err := run(context.Background(), []string{"policy", "evaluate", "--token=never-store"}, &output, &output); err == nil || !strings.Contains(err.Error(), "cannot carry secrets") {
		t.Fatalf("secret argument error = %v", err)
	}
	databasePath := filepath.Join(t.TempDir(), "control-plane.db")
	if err := run(context.Background(), []string{"db", "migrate", "--database", databasePath, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.Exec(`DELETE FROM schema_migrations WHERE version = 10`); err != nil {
		t.Fatal(err)
	}
	err = run(context.Background(), []string{
		"policy", "evaluate", "--database", databasePath,
		"--assessment-id", "assessment-1", "--rule-key", "assigned-default", "--rule-version-id", "rule-version-1",
	}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "current schema") {
		t.Fatalf("outdated schema error = %v", err)
	}
	assertCLIQueryCount(t, database, "policy_evaluations", 0)
}

func TestProposalEditAndDecideUseBoundedFilesWithoutPublication(t *testing.T) {
	databasePath, assessmentID, ruleVersionID := seedPolicyEvaluationCandidate(t)
	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"policy", "evaluate", "--database", databasePath,
		"--assessment-id", assessmentID, "--rule-key", "assigned-default", "--rule-version-id", ruleVersionID,
	}, &output, &output); err != nil {
		t.Fatal(err)
	}
	var evaluation struct {
		Evaluation struct {
			ProposalID string
		}
	}
	if err := json.Unmarshal(output.Bytes(), &evaluation); err != nil {
		t.Fatal(err)
	}
	if evaluation.Evaluation.ProposalID == "" {
		t.Fatalf("policy evaluation output = %s", output.String())
	}
	directory := t.TempDir()
	bodyPath := filepath.Join(directory, "body.txt")
	commentsPath := filepath.Join(directory, "comments.json")
	reasonPath := filepath.Join(directory, "reason.txt")
	if err := os.WriteFile(bodyPath, []byte("Human-confirmed change request.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commentsPath, []byte(`[{"path":"internal/example.go","line":2,"body":"Guard nil input."}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reasonPath, []byte("Confirmed after inspection.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(context.Background(), []string{
		"proposal", "edit", "--database", databasePath, "--proposal-id", evaluation.Evaluation.ProposalID,
		"--body-file", bodyPath, "--inline-comments-file", commentsPath,
	}, &output, &output); err != nil {
		t.Fatal(err)
	}
	var edit struct {
		ProposalRevisionID string
		RevisionNumber     int
	}
	if err := json.Unmarshal(output.Bytes(), &edit); err != nil {
		t.Fatal(err)
	}
	if edit.ProposalRevisionID == "" || edit.RevisionNumber != 2 {
		t.Fatalf("proposal edit output = %s", output.String())
	}
	output.Reset()
	if err := run(context.Background(), []string{
		"proposal", "decide", "--database", databasePath, "--proposal-revision-id", edit.ProposalRevisionID,
		"--decision", "reject", "--actor-id", "local-user", "--reason-file", reasonPath,
	}, &output, &output); err != nil {
		t.Fatal(err)
	}
	var decision struct {
		DecisionID     string `json:"decision_id"`
		Created        bool   `json:"created"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.Unmarshal(output.Bytes(), &decision); err != nil {
		t.Fatal(err)
	}
	if decision.DecisionID == "" || !decision.Created || decision.IdempotencyKey == "" {
		t.Fatalf("proposal decide output = %s", output.String())
	}
	output.Reset()
	if err := run(context.Background(), []string{
		"proposal", "decide", "--database", databasePath, "--proposal-revision-id", edit.ProposalRevisionID,
		"--decision", "reject", "--actor-id", "local-user", "--reason-file", reasonPath,
	}, &output, &output); err != nil {
		t.Fatal(err)
	}
	var replay struct {
		DecisionID string `json:"decision_id"`
		Created    bool   `json:"created"`
	}
	if err := json.Unmarshal(output.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Created || replay.DecisionID != decision.DecisionID {
		t.Fatalf("proposal decide replay output = %s", output.String())
	}

	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	assertCLIQueryCount(t, database, "proposal_revisions", 2)
	assertCLIQueryCount(t, database, "decisions", 1)
	for _, table := range []string{"publication_effects", "publication_attempts", "jobs", "domain_events", "outbox"} {
		assertCLIQueryCount(t, database, table, 0)
	}
}

func TestProposalManualControlsRejectSecretsAndInvalidFiles(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"proposal", "edit", "--token=never-store"}, &output, &output); err == nil || !strings.Contains(err.Error(), "cannot carry secrets") {
		t.Fatalf("secret edit argument error = %v", err)
	}
	if err := run(context.Background(), []string{"proposal", "decide", "--api-key=never-store"}, &output, &output); err == nil || !strings.Contains(err.Error(), "cannot carry secrets") {
		t.Fatalf("secret decide argument error = %v", err)
	}
	if err := run(context.Background(), []string{"proposal", "decide", "--proposal-revision-id", "proposal-revision-1", "--decision", "approve", "--actor-id", "ghp_neverstore"}, &output, &output); err == nil || !strings.Contains(err.Error(), "secret value") {
		t.Fatalf("secret actor error = %v", err)
	}
	directory := t.TempDir()
	commentsPath := filepath.Join(directory, "comments.json")
	if err := os.WriteFile(commentsPath, []byte(`[{"token":"never-store"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), []string{
		"proposal", "edit", "--proposal-id", "proposal-1", "--body-file", commentsPath, "--inline-comments-file", commentsPath,
	}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "secret-bearing") || strings.Contains(err.Error(), "never-store") {
		t.Fatalf("secret comments error = %v", err)
	}
}

func TestProposalPublishRequiresSimulationAcknowledgementAndRecordsDisabledEffect(t *testing.T) {
	databasePath, proposalRevisionID := seedApprovedProposalRevision(t)
	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"proposal", "publish", "--database", databasePath, "--proposal-revision-id", proposalRevisionID,
	}, &output, &output); err == nil || !strings.Contains(err.Error(), "--simulate") {
		t.Fatalf("publish without acknowledgement error = %v", err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	for _, table := range []string{"publication_effects", "publication_attempts", "jobs", "domain_events", "outbox"} {
		assertCLIQueryCount(t, database, table, 0)
	}

	output.Reset()
	if err := run(context.Background(), []string{
		"proposal", "publish", "--database", databasePath, "--proposal-revision-id", proposalRevisionID, "--simulate",
	}, &output, &output); err != nil {
		t.Fatal(err)
	}
	var result struct {
		EffectID        string `json:"effect_id"`
		PublicationMode string `json:"publication_mode"`
		Created         bool   `json:"created"`
		Job             *struct {
			ID      string `json:"id"`
			Created bool   `json:"created"`
		} `json:"job"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.EffectID == "" || !result.Created || result.PublicationMode != "disabled" || result.Job != nil {
		t.Fatalf("disabled publication output = %s", output.String())
	}
	assertCLIQueryCount(t, database, "publication_effects", 1)
	for _, table := range []string{"publication_attempts", "jobs", "domain_events", "outbox"} {
		assertCLIQueryCount(t, database, table, 0)
	}
}

func TestProposalPublishSchedulesOneSimulatedJob(t *testing.T) {
	databasePath, proposalRevisionID := seedApprovedProposalRevision(t)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.Exec(`UPDATE system_state SET value = 'simulated', updated_at_us = 100 WHERE key = 'publication_mode'`); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command := []string{
		"proposal", "publish", "--database", databasePath, "--proposal-revision-id", proposalRevisionID, "--simulate",
	}
	if err := run(context.Background(), command, &output, &output); err != nil {
		t.Fatal(err)
	}
	var first struct {
		EffectID        string `json:"effect_id"`
		PublicationMode string `json:"publication_mode"`
		Created         bool   `json:"created"`
		Job             *struct {
			ID      string `json:"id"`
			Created bool   `json:"created"`
		} `json:"job"`
	}
	if err := json.Unmarshal(output.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.EffectID == "" || !first.Created || first.PublicationMode != "simulated" || first.Job == nil || first.Job.ID == "" || !first.Job.Created {
		t.Fatalf("simulated publication output = %s", output.String())
	}
	assertCLIQueryCount(t, database, "publication_effects", 1)
	assertCLIQueryCount(t, database, "jobs", 1)
	var kind, payload string
	if err := database.QueryRow(`SELECT kind, payload_json FROM jobs`).Scan(&kind, &payload); err != nil {
		t.Fatal(err)
	}
	if kind != publishworker.SimulateJobKind || !strings.Contains(payload, first.EffectID) || strings.Contains(payload, proposalRevisionID) {
		t.Fatalf("simulated publication job kind=%q payload=%s", kind, payload)
	}

	output.Reset()
	if err := run(context.Background(), command, &output, &output); err != nil {
		t.Fatal(err)
	}
	var replay struct {
		EffectID string `json:"effect_id"`
		Created  bool   `json:"created"`
		Job      *struct {
			ID      string `json:"id"`
			Created bool   `json:"created"`
		} `json:"job"`
	}
	if err := json.Unmarshal(output.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.EffectID != first.EffectID || replay.Created || replay.Job == nil || replay.Job.ID != first.Job.ID || replay.Job.Created {
		t.Fatalf("simulated publication replay output = %s", output.String())
	}
	assertCLIQueryCount(t, database, "publication_effects", 1)
	assertCLIQueryCount(t, database, "jobs", 1)
	for _, table := range []string{"publication_attempts", "domain_events", "outbox"} {
		assertCLIQueryCount(t, database, table, 0)
	}
}

func TestProposalPublishRecordsEnabledIntentWithoutDispatch(t *testing.T) {
	databasePath, proposalRevisionID := seedApprovedProposalRevision(t)
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.Exec(`UPDATE system_state SET value = 'enabled', updated_at_us = 100 WHERE key = 'publication_mode'`); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err = run(context.Background(), []string{
		"proposal", "publish", "--database", databasePath, "--proposal-revision-id", proposalRevisionID, "--simulate",
	}, &output, &output)
	if err != nil {
		t.Fatalf("enabled publication error = %v", err)
	}
	var result struct {
		PublicationMode string `json:"publication_mode"`
		Created         bool   `json:"created"`
		Job             any    `json:"job"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.PublicationMode != "enabled" || !result.Created || result.Job != nil {
		t.Fatalf("enabled publication output = %s", output.String())
	}
	assertCLIQueryCount(t, database, "publication_effects", 1)
	for _, table := range []string{"publication_attempts", "jobs", "domain_events", "outbox"} {
		assertCLIQueryCount(t, database, table, 0)
	}
}

func TestProposalPublishRejectsSecretsInvalidIDsAndOutdatedSchema(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"proposal", "publish", "--token=never-store"}, &output, &output); err == nil || !strings.Contains(err.Error(), "cannot carry secrets") {
		t.Fatalf("secret argument error = %v", err)
	}
	if err := run(context.Background(), []string{
		"proposal", "publish", "--proposal-revision-id", " bad", "--simulate",
	}, &output, &output); err == nil || !strings.Contains(err.Error(), "proposal revision ID") {
		t.Fatalf("invalid ID error = %v", err)
	}
	databasePath := filepath.Join(t.TempDir(), "control-plane.db")
	if err := run(context.Background(), []string{"db", "migrate", "--database", databasePath, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.Exec(`DELETE FROM schema_migrations WHERE version = 10`); err != nil {
		t.Fatal(err)
	}
	err = run(context.Background(), []string{
		"proposal", "publish", "--database", databasePath, "--proposal-revision-id", "proposal_revision-1", "--simulate",
	}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "current schema") {
		t.Fatalf("outdated schema error = %v", err)
	}
}

func seedApprovedProposalRevision(t *testing.T) (databasePath, proposalRevisionID string) {
	t.Helper()
	databasePath, assessmentID, ruleVersionID := seedPolicyEvaluationCandidate(t)
	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"policy", "evaluate", "--database", databasePath,
		"--assessment-id", assessmentID, "--rule-key", "assigned-default", "--rule-version-id", ruleVersionID,
	}, &output, &output); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT id FROM proposal_revisions ORDER BY created_at_us DESC LIMIT 1`).Scan(&proposalRevisionID); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(context.Background(), []string{
		"proposal", "decide", "--database", databasePath, "--proposal-revision-id", proposalRevisionID,
		"--decision", "approve", "--actor-id", "local-user",
	}, &output, &output); err != nil {
		t.Fatal(err)
	}
	return databasePath, proposalRevisionID
}

func seedPolicyEvaluationCandidate(t *testing.T) (databasePath, assessmentID, ruleVersionID string) {
	t.Helper()
	ctx := context.Background()
	databasePath = filepath.Join(t.TempDir(), "control-plane.db")
	var output bytes.Buffer
	if err := run(ctx, []string{"db", "migrate", "--database", databasePath, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	headSHA := strings.Repeat("a", 40)
	baseSHA := strings.Repeat("b", 40)
	digest := strings.Repeat("c", 64)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO connections(
 id, provider, mode, auth_kind, api_base_url, account_login, account_database_id,
 credential_ref_kind, credential_locator, state, permissions_json, created_at_us, updated_at_us)
VALUES ('connection-1', 'github', 'local_user', 'github_cli', 'https://api.github.com', 'reviewer', 9001,
 'github_cli', 'github-cli', 'active', '{"pull_requests":"read"}', 1, 1)`, nil},
		{`INSERT INTO repositories(id, github_node_id, full_name, owner_login, name, created_at_us, updated_at_us, github_id)
VALUES ('repo-1', 'R_1', 'owner/repo', 'owner', 'repo', 1, 1, 1001)`, nil},
		{`INSERT INTO connection_repositories(
 connection_id, repository_id, github_repository_id, github_node_id, access_state, permissions_json, created_at_us, updated_at_us)
VALUES ('connection-1', 'repo-1', 1001, 'R_1', 'active', '{"pull":"read"}', 2, 2)`, nil},
		{`INSERT INTO pull_requests(id, repository_id, github_id, number, title, author_login, html_url, state, created_at_us, updated_at_us)
VALUES ('pr-1', 'repo-1', 2001, 42, 'Policy fixture', 'author', 'https://github.com/owner/repo/pull/42', 'open', 3, 3)`, nil},
		{`INSERT INTO pull_request_observations(
 id, connection_id, repository_id, pull_request_id, revision_id, head_sha, base_sha,
 source_kind, source_priority, facts_format_version, facts_sha256, title, author_login,
 author_database_id, body_sha256, labels_json, is_draft, base_ref, requested_reviewers_json,
 relationship_set_json, github_state, github_updated_at_us, observed_at_us, created_at_us)
VALUES ('observation-1', 'connection-1', 'repo-1', 'pr-1', NULL, ?, ?,
 'direct_refresh', 30, 1, ?, 'Policy fixture', 'author', 8001, ?, '[]', 0, 'main', '[]', '[]',
 'open', 10, 10, 10)`, []any{headSHA, baseSHA, digest, digest}},
		{`INSERT INTO pull_request_projection_state(
 pull_request_id, repository_id, connection_id, current_revision_id, current_observation_id, freshness, updated_at_us)
VALUES ('pr-1', 'repo-1', 'connection-1', NULL, 'observation-1', 'fresh', 10)`, nil},
	} {
		if _, err := database.ExecContext(ctx, statement.query, statement.args...); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := storagesqlite.Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	revision, err := canonical.Build(canonical.Input{
		HeadSHA: headSHA, BaseSHA: baseSHA, Complete: true,
		Files: []canonical.FileChange{{
			Path: "internal/example.go", Status: "modified", BaseBlobSHA: baseSHA, HeadBlobSHA: headSHA,
			BaseMode: "100644", HeadMode: "100644", Patch: []byte("-old\\n+new\\n"), PatchPresent: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachCanonicalRevision(ctx, storagesqlite.CanonicalRevisionInput{
		ConnectionID: "connection-1", ObservationID: "observation-1", HeadSHA: headSHA, BaseSHA: baseSHA,
		IdentityKey: revision.IdentityKey, ManifestSHA256: revision.ManifestSHA256, ManifestJSON: revision.Manifest,
		EntryCount: revision.EntryCount, AttachedAt: time.Unix(20, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	profile, err := store.CreateReviewProfileVersion(ctx, storagesqlite.CreateReviewProfileVersionInput{
		ProfileKey: "default", Version: 1, Name: "Default", Instructions: "Review carefully.", SettingsJSON: []byte(`{}`), CreatedAt: time.Unix(21, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := store.PrepareReviewRun(ctx, storagesqlite.PrepareReviewRunInput{
		ConnectionID: "connection-1", PullRequestID: "pr-1", ProfileID: profile.ProfileID, ProfileVersionID: profile.VersionID,
		TriggerKind: "manual", TriggerSHA256: strings.Repeat("d", 64), EngineKind: "cli", EngineConfigJSON: []byte(`{}`),
		AccessMode: "diff_only", RequestedAt: time.Unix(22, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := assessment.Validate([]byte(`{"version":1,"verdict":"changes_required","summary":"Nil input can panic.","confidence":"high","limitations":[],"coverage":{"status":"complete","changed_files_total":1,"reviewed_files":1,"omitted":[]},"findings":[{"client_id":"nil-input","severity":"high","category":"correctness","message":"Guard optional input.","anchor":{"path":"internal/example.go","start_line":2,"end_line":2,"side":"RIGHT","sha":"`+headSHA+`"}}]}`), assessment.RevisionEvidence{
		HeadSHA: headSHA, BaseSHA: baseSHA, Files: []assessment.FileEvidence{{Path: "internal/example.go", Right: []assessment.LineRange{{Start: 1, End: 2}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := store.RecordAssessment(ctx, storagesqlite.RecordAssessmentInput{RunID: prepared.RunID, Result: validated, RecordedAt: time.Unix(23, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	policySet, err := store.CreatePolicySetGeneration(ctx, storagesqlite.PolicySetGenerationInput{
		Generation: 1,
		Rules: []storagesqlite.WatchRuleVersionInput{{
			RuleKey: "assigned-default", Enabled: true, Priority: 1, TriggerKind: "manual", ExternalActionPolicy: "require_confirmation",
			ProfileID: profile.ProfileID, ProfileVersionID: profile.VersionID, MatchJSON: []byte(`{}`), ReviewJSON: []byte(`{}`),
			PublicationJSON: []byte(`{"allow_automatic_approval":false,"matrix":{"changes_required":"propose_changes"}}`),
		}},
		CreatedAt: time.Unix(24, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return databasePath, recorded.AssessmentID, policySet.RuleVersions[0].VersionID
}

func TestSameFileDetectsSymlinkAlias(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.db")
	if err := os.WriteFile(source, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "alias.db")
	if err := os.Symlink(source, alias); err != nil {
		t.Fatal(err)
	}

	same, err := sameFile(alias, source)
	if err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Fatal("sameFile() = false for symlink alias")
	}
}

func TestLegacyImportRequiresExplicitBackupAndSourceID(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"legacy", "import"}, &output, &output); err == nil || !strings.Contains(err.Error(), "--source") {
		t.Fatalf("legacy import without source error = %v", err)
	}
	output.Reset()
	if err := run(context.Background(), []string{"legacy", "import", "--source", "backup.db"}, &output, &output); err == nil || !strings.Contains(err.Error(), "--source-id") {
		t.Fatalf("legacy import without source ID error = %v", err)
	}
}

func TestLegacyImportApplyDoesNotCreateMissingTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "missing.db")
	var output bytes.Buffer
	err := run(context.Background(), []string{
		"legacy", "import", "--source", filepath.Join(t.TempDir(), "missing-backup.db"),
		"--source-id", "legacy-test", "--database", target, "--apply",
	}, &output, &output)
	if err == nil {
		t.Fatal("legacy import accepted missing source")
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("legacy import created target: %v", statErr)
	}
}

func TestGitHubReconcileRequiresShadow(t *testing.T) {
	var output bytes.Buffer
	err := run(context.Background(), []string{"github", "reconcile"}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "--shadow") {
		t.Fatalf("github reconcile error = %v", err)
	}
}

func TestGitHubHydrateRequiresSafeTargetFlags(t *testing.T) {
	var output bytes.Buffer
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "shadow", args: []string{"github", "hydrate"}, want: "--shadow"},
		{name: "coordinates", args: []string{"github", "hydrate", "--shadow"}, want: "repository coordinates"},
		{name: "number", args: []string{"github", "hydrate", "--shadow", "--owner", "acme", "--repository", "widgets"}, want: "pull request number"},
		{name: "timeout", args: []string{"github", "hydrate", "--shadow", "--owner", "acme", "--repository", "widgets", "--number", "42", "--http-timeout", "0s"}, want: "positive HTTP timeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output.Reset()
			err := run(context.Background(), test.args, &output, &output)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestGitHubHydrateDoesNotCreateMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	t.Setenv("GITHUB_TOKEN", "test-secret")
	var output bytes.Buffer
	err := run(context.Background(), []string{
		"github", "hydrate", "--shadow", "--database", path,
		"--owner", "acme", "--repository", "widgets", "--number", "42",
	}, &output, &output)
	if err == nil {
		t.Fatal("github hydrate accepted a missing database")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("github hydrate created target: %v", statErr)
	}
}

func TestGitHubHydrateAttachesFixtureWithoutSideEffects(t *testing.T) {
	const (
		token = "test-secret"
		head  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		base  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		blob  = "cccccccccccccccccccccccccccccccccccccccc"
	)
	var methods []string
	var methodsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		methodsMu.Lock()
		methods = append(methods, request.Method)
		methodsMu.Unlock()
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/user":
			_, _ = response.Write([]byte(`{"id":9001,"node_id":"U_9001","login":"reviewer"}`))
		case "/search/issues":
			if strings.Contains(request.URL.Query().Get("q"), "review-requested:reviewer") {
				_, _ = response.Write([]byte(fmt.Sprintf(`{"total_count":1,"incomplete_results":false,"items":[{"number":42,"repository_url":%q,"pull_request":{}}]}`, serverURLForTest(request)+"/repos/acme/widgets")))
				return
			}
			_, _ = response.Write([]byte(`{"total_count":0,"incomplete_results":false,"items":[]}`))
		case "/repos/acme/widgets/pulls/42":
			if request.Header.Get("Accept") == "application/vnd.github.diff" {
				_, _ = response.Write([]byte("diff --git a/new.txt b/new.txt\n@@ -0,0 +1 @@\n+new\n"))
				return
			}
			_, _ = response.Write([]byte(fmt.Sprintf(`{
              "id":501,"node_id":"PR_501","number":42,
              "html_url":"https://github.com/acme/widgets/pull/42",
              "title":"Fixture PR","body":"Fixture details",
              "user":{"id":9,"node_id":"U_9","login":"author"},
              "state":"open","merged":false,"draft":false,"updated_at":"2026-07-21T08:00:00Z",
              "head":{"sha":%q},
              "base":{"sha":%q,"ref":"main","repo":{"id":77,"node_id":"R_77","full_name":"acme/widgets"}},
              "labels":[],"requested_reviewers":[{"id":9001,"node_id":"U_9001","login":"reviewer"}]
            }`, head, base)))
		case "/repos/acme/widgets/pulls/42/files":
			if request.URL.Query().Get("page") != "1" {
				t.Errorf("file page = %q", request.URL.Query().Get("page"))
			}
			_, _ = response.Write([]byte(fmt.Sprintf(`[{"filename":"new.txt","status":"added","sha":%q,"patch":"+new\\n"}]`, blob)))
		case "/repos/acme/widgets/git/trees/" + base:
			_, _ = response.Write([]byte(`{"truncated":false,"tree":[]}`))
		case "/repos/acme/widgets/git/trees/" + head:
			_, _ = response.Write([]byte(fmt.Sprintf(`{"truncated":false,"tree":[{"path":"new.txt","mode":"100644","type":"blob","sha":%q}]}`, blob)))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	databasePath := filepath.Join(t.TempDir(), "control-plane.db")
	var output bytes.Buffer
	if err := run(context.Background(), []string{"db", "migrate", "--database", databasePath, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", token)
	if err := run(context.Background(), []string{"github", "reconcile", "--shadow", "--database", databasePath, "--api-url", server.URL, "--connection-id", "fixture"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(context.Background(), []string{
		"github", "hydrate", "--shadow", "--database", databasePath, "--api-url", server.URL,
		"--connection-id", "fixture", "--owner", "acme", "--repository", "widgets", "--number", "42",
	}, &output, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"EntryCount": 1`) || strings.Contains(output.String(), token) {
		t.Fatalf("hydrate output = %s", output.String())
	}

	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	assertCLIQueryCount(t, database, "revision_manifests", 1)
	assertCLIQueryCount(t, database, "observation_revision_links", 1)
	assertCLIQueryCount(t, database, "jobs", 0)
	assertCLIQueryCount(t, database, "domain_events", 0)
	assertCLIQueryCount(t, database, "outbox", 0)
	methodsMu.Lock()
	defer methodsMu.Unlock()
	for _, method := range methods {
		if method != http.MethodGet {
			t.Fatalf("GitHub fixture saw mutating method %q", method)
		}
	}
}

func TestGitHubReconcileDoesNotCreateMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	t.Setenv("GITHUB_TOKEN", "test-secret")
	var output bytes.Buffer
	err := run(context.Background(), []string{"github", "reconcile", "--shadow", "--database", path}, &output, &output)
	if err == nil {
		t.Fatal("github reconcile accepted missing database")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("github reconcile created target: %v", statErr)
	}
}

func TestGitHubReconcileProjectsFixtureWithoutSideEffects(t *testing.T) {
	const token = "test-secret"
	var fixtureMu sync.Mutex
	var methods []string
	reviewRequested := true
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		fixtureMu.Lock()
		methods = append(methods, request.Method)
		requested := reviewRequested
		fixtureMu.Unlock()
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/user":
			_, _ = response.Write([]byte(`{"id":9001,"node_id":"U_9001","login":"reviewer"}`))
		case "/search/issues":
			query := request.URL.Query().Get("q")
			if strings.Contains(query, "review-requested:reviewer") && requested {
				_, _ = response.Write([]byte(fmt.Sprintf(`{
                  "total_count":1,"incomplete_results":false,
                  "items":[{"number":42,"repository_url":%q,"pull_request":{}}]
                }`, serverURLForTest(request)+"/repos/acme/widgets")))
				return
			}
			if strings.Contains(query, "review-requested:reviewer") {
				_, _ = response.Write([]byte(`{"total_count":0,"incomplete_results":false,"items":[]}`))
				return
			}
			if !strings.Contains(query, "author:reviewer") {
				t.Errorf("unexpected search query %q", query)
			}
			_, _ = response.Write([]byte(`{"total_count":0,"incomplete_results":false,"items":[]}`))
		case "/repos/acme/widgets/pulls/42":
			reviewers := `[]`
			if requested {
				reviewers = `[{"id":9001,"node_id":"U_9001","login":"reviewer"}]`
			}
			_, _ = response.Write([]byte(fmt.Sprintf(`{
              "id":501,"node_id":"PR_501","number":42,
              "html_url":"https://github.com/acme/widgets/pull/42",
              "title":"Fixture PR","body":"Fixture details",
              "user":{"id":9,"node_id":"U_9","login":"author"},
              "state":"open","merged":false,"draft":false,"updated_at":"2026-07-21T08:00:00Z",
              "head":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
              "base":{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","ref":"main",
                "repo":{"id":77,"node_id":"R_77","full_name":"acme/widgets"}},
              "labels":[{"name":"safe"}],
			  "requested_reviewers":%s
			}`, reviewers)))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	databasePath := filepath.Join(t.TempDir(), "control-plane.db")
	var output bytes.Buffer
	if err := run(context.Background(), []string{"db", "migrate", "--database", databasePath, "--apply"}, &output, &output); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", token)
	command := []string{
		"github", "reconcile", "--shadow", "--database", databasePath,
		"--api-url", server.URL, "--connection-id", "fixture",
	}
	for runNumber := 1; runNumber <= 2; runNumber++ {
		output.Reset()
		if err := run(context.Background(), command, &output, &output); err != nil {
			t.Fatalf("reconcile run %d: %v", runNumber, err)
		}
		if strings.Contains(output.String(), token) || strings.Count(output.String(), `"state": "complete"`) != 2 {
			t.Fatalf("reconcile run %d output = %s", runNumber, output.String())
		}
	}
	fixtureMu.Lock()
	reviewRequested = false
	fixtureMu.Unlock()
	output.Reset()
	if err := run(context.Background(), command, &output, &output); err != nil {
		t.Fatalf("reconcile closure run: %v", err)
	}
	if strings.Contains(output.String(), token) || strings.Count(output.String(), `"state": "complete"`) != 2 {
		t.Fatalf("reconcile closure output = %s", output.String())
	}
	fixtureMu.Lock()
	for _, method := range methods {
		if method != http.MethodGet {
			fixtureMu.Unlock()
			t.Fatalf("GitHub fixture saw mutating method %q", method)
		}
	}
	fixtureMu.Unlock()

	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	assertCLIQueryCount(t, database, "connections", 1)
	assertCLIQueryCount(t, database, "reconciliation_generations", 6)
	assertCLIQueryCount(t, database, "pull_request_observations", 2)
	assertCLIQueryCount(t, database, "pr_relationships", 1)
	assertCLIQueryCount(t, database, "reconciliation_generation_items", 3)
	assertCLIQueryCount(t, database, "jobs", 0)
	assertCLIQueryCount(t, database, "domain_events", 0)
	assertCLIQueryCount(t, database, "outbox", 0)
	var publicationMode string
	if err := database.QueryRow(`SELECT value FROM system_state WHERE key = 'publication_mode'`).Scan(&publicationMode); err != nil {
		t.Fatal(err)
	}
	if publicationMode != "disabled" {
		t.Fatalf("publication mode = %q", publicationMode)
	}
	var activeRelationships int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pr_relationships WHERE active_until_us IS NULL`).Scan(&activeRelationships); err != nil {
		t.Fatal(err)
	}
	if activeRelationships != 0 {
		t.Fatalf("active relationships = %d, want 0 after direct removal proof", activeRelationships)
	}
}

func serverURLForTest(request *http.Request) string { return "http://" + request.Host }

func assertCLIQueryCount(t *testing.T, database *sql.DB, table string, want int) {
	t.Helper()
	var got int
	if err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s rows = %d, want %d", table, got, want)
	}
}
