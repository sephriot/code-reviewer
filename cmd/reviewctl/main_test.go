package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
	if !strings.Contains(output.String(), `"current": 6`) {
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
