package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIReviewPassesRawJSONThroughStdin(t *testing.T) {
	adapter := newTestCLI(t, "echo")
	want := json.RawMessage(`{"revision":{"head_sha":"abc"},"instructions":"review"}`)
	result, err := adapter.Review(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(result.Stdout); got != string(want) {
		t.Fatalf("stdout = %q, want exact input %q", got, want)
	}
	if result.Executable == "" || !filepath.IsAbs(result.Executable) {
		t.Fatalf("resolved executable = %q, want absolute path", result.Executable)
	}
	if result.Duration < 0 {
		t.Fatalf("duration = %s", result.Duration)
	}
}

func TestCLIReviewUsesFreshMinimalEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "must-not-reach-engine")
	t.Setenv("GH_TOKEN", "must-not-reach-engine")
	adapter := newTestCLI(t, "environment")
	result, err := adapter.Review(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var environment struct {
		GitHubToken string `json:"github_token"`
		GHToken     string `json:"gh_token"`
		Home        string `json:"home"`
		Temporary   string `json:"temporary"`
		Working     string `json:"working"`
		Language    string `json:"language"`
	}
	if err := json.Unmarshal(result.Stdout, &environment); err != nil {
		t.Fatalf("decode helper output: %v; stdout=%q", err, result.Stdout)
	}
	if environment.GitHubToken != "" || environment.GHToken != "" {
		t.Fatalf("engine inherited GitHub credential environment: %#v", environment)
	}
	if environment.Home == "" || environment.Home != environment.Temporary || environment.Home != environment.Working {
		t.Fatalf("engine work environment = %#v", environment)
	}
	if environment.Language != "C" {
		t.Fatalf("LANG = %q, want C", environment.Language)
	}
	if _, err := os.Stat(environment.Working); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("engine work directory remains after review: %v", err)
	}
}

func TestCLIReviewBoundsAndCancelsOutput(t *testing.T) {
	for _, mode := range []string{"stdout-overflow", "stderr-overflow"} {
		t.Run(mode, func(t *testing.T) {
			adapter, err := New(Config{
				Argv:           helperArgv(mode),
				Timeout:        5 * time.Second,
				MaxStdoutBytes: 4,
				MaxStderrBytes: 4,
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := adapter.Review(context.Background(), json.RawMessage(`{}`))
			if !errors.Is(err, ErrOutputLimit) {
				t.Fatalf("Review error = %v, want output limit", err)
			}
			if len(result.Stdout) > 4 || len(result.Stderr) > 4 {
				t.Fatalf("unbounded output: stdout=%d stderr=%d", len(result.Stdout), len(result.Stderr))
			}
		})
	}
}

func TestCLIReviewCancelsOnCallerDeadline(t *testing.T) {
	adapter := newTestCLI(t, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := adapter.Review(ctx, json.RawMessage(`{}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Review error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Review cancellation took %s", elapsed)
	}
}

func TestCLIReviewReportsExitWithoutLeakingOutput(t *testing.T) {
	adapter := newTestCLI(t, "exit")
	result, err := adapter.Review(context.Background(), json.RawMessage(`{}`))
	if !errors.Is(err, ErrEngineExit) {
		t.Fatalf("Review error = %v, want engine exit", err)
	}
	if strings.Contains(err.Error(), "engine diagnostic") {
		t.Fatalf("error leaked engine stderr: %v", err)
	}
	if got := string(result.Stderr); got != "engine diagnostic" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestNewRejectsUnsafeConfig(t *testing.T) {
	for name, config := range map[string]Config{
		"missing argv":        {},
		"github environment":  {Argv: helperArgv("echo"), Environment: map[string]string{"GITHUB_TOKEN": "secret"}},
		"unknown environment": {Argv: helperArgv("echo"), Environment: map[string]string{"SECRET": "secret"}},
		"negative timeout":    {Argv: helperArgv("echo"), Timeout: -time.Second},
		"oversized output":    {Argv: helperArgv("echo"), MaxStdoutBytes: maxOutputBytes + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(config); err == nil {
				t.Fatal("New accepted unsafe configuration")
			}
		})
	}
}

func TestCLIReviewRejectsInvalidJSONBeforeExecution(t *testing.T) {
	adapter := newTestCLI(t, "exit")
	if _, err := adapter.Review(context.Background(), json.RawMessage(`{"broken"`)); err == nil {
		t.Fatal("Review accepted invalid JSON")
	}
}

func newTestCLI(t *testing.T, mode string) *CLI {
	t.Helper()
	adapter, err := New(Config{
		Argv:        helperArgv(mode),
		Environment: map[string]string{"LANG": "C"},
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func helperArgv(mode string) []string {
	return []string{os.Args[0], "-test.run=TestCLIHelperProcess", "--", "engine-helper", mode}
}

func TestCLIHelperProcess(t *testing.T) {
	index := -1
	for position, argument := range os.Args {
		if argument == "engine-helper" {
			index = position
			break
		}
	}
	if index < 0 || index+1 >= len(os.Args) {
		return
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(2)
	}
	switch os.Args[index+1] {
	case "echo":
		_, _ = os.Stdout.Write(input)
	case "environment":
		working, _ := os.Getwd()
		home, _ := filepath.EvalSymlinks(os.Getenv("HOME"))
		temporary, _ := filepath.EvalSymlinks(os.Getenv("TMPDIR"))
		working, _ = filepath.EvalSymlinks(working)
		_, _ = fmt.Fprintf(os.Stdout, `{"github_token":%q,"gh_token":%q,"home":%q,"temporary":%q,"working":%q,"language":%q}`,
			os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN"), home, temporary, working, os.Getenv("LANG"))
	case "stdout-overflow":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", 128))
	case "stderr-overflow":
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("x", 128))
	case "sleep":
		time.Sleep(5 * time.Second)
	case "exit":
		_, _ = fmt.Fprint(os.Stderr, "engine diagnostic")
		os.Exit(23)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q", os.Args[index+1])
		os.Exit(2)
	}
	os.Exit(0)
}
