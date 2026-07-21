// Package engine runs trusted review-engine CLIs with a deliberately narrow
// process boundary. It never supplies GitHub credentials or publication
// capability to a child process.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTimeout bounds a single CLI assessment invocation.
	DefaultTimeout = 10 * time.Minute
	// DefaultMaxInputBytes bounds the JSON bundle supplied to a CLI.
	DefaultMaxInputBytes = 16 << 20
	// DefaultMaxStdoutBytes matches the maximum assessment document accepted by
	// the assessment contract.
	DefaultMaxStdoutBytes = 1 << 20
	// DefaultMaxStderrBytes retains useful diagnostics without retaining an
	// unbounded amount of engine output.
	DefaultMaxStderrBytes = 64 << 10

	maxTimeout     = time.Hour
	maxInputBytes  = 64 << 20
	maxOutputBytes = 8 << 20
)

var (
	// ErrOutputLimit means the engine exceeded a configured output bound and
	// was cancelled. Result contains only the bounded prefix.
	ErrOutputLimit = errors.New("engine output limit exceeded")
	// ErrEngineExit means the engine exited unsuccessfully. Stderr remains in
	// Result so the caller can persist it under its own redaction policy.
	ErrEngineExit = errors.New("engine process exited unsuccessfully")
)

// Config defines a trusted local CLI engine. Argv is executed directly and
// never interpreted by a shell. Environment permits only locale and PATH
// overrides; it is never merged with the daemon environment.
type Config struct {
	Argv           []string
	Environment    map[string]string
	Timeout        time.Duration
	MaxInputBytes  int
	MaxStdoutBytes int
	MaxStderrBytes int
}

// Result is the bounded raw output of a completed CLI invocation. Stdout is
// intentionally not parsed here; assessment owns output validation.
type Result struct {
	Stdout     []byte
	Stderr     []byte
	Executable string
	Duration   time.Duration
}

// Adapter is the provider-neutral CLI assessment boundary.
type Adapter interface {
	Review(context.Context, json.RawMessage) (Result, error)
}

// CLI is a validated direct-argv engine adapter.
type CLI struct {
	argv           []string
	environment    map[string]string
	timeout        time.Duration
	maxInputBytes  int
	maxStdoutBytes int
	maxStderrBytes int
	executable     string
}

// New validates a trusted CLI configuration before it can be used.
func New(config Config) (*CLI, error) {
	if len(config.Argv) == 0 || strings.TrimSpace(config.Argv[0]) == "" {
		return nil, errors.New("engine argv is required")
	}
	argv := make([]string, len(config.Argv))
	for index, argument := range config.Argv {
		if strings.ContainsRune(argument, 0) {
			return nil, errors.New("engine argv cannot contain NUL")
		}
		argv[index] = argument
	}
	executable, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, fmt.Errorf("resolve engine executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve engine executable path: %w", err)
	}
	environment, err := validateEnvironment(config.Environment)
	if err != nil {
		return nil, err
	}
	timeout, err := normalizeTimeout(config.Timeout)
	if err != nil {
		return nil, err
	}
	maxInput, err := normalizeLimit("engine input", config.MaxInputBytes, DefaultMaxInputBytes)
	if err != nil || maxInput > maxInputBytes {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("engine input limit must not exceed %d bytes", maxInputBytes)
	}
	maxStdout, err := normalizeLimit("engine stdout", config.MaxStdoutBytes, DefaultMaxStdoutBytes)
	if err != nil || maxStdout > maxOutputBytes {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("engine stdout limit must not exceed %d bytes", maxOutputBytes)
	}
	maxStderr, err := normalizeLimit("engine stderr", config.MaxStderrBytes, DefaultMaxStderrBytes)
	if err != nil || maxStderr > maxOutputBytes {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("engine stderr limit must not exceed %d bytes", maxOutputBytes)
	}
	return &CLI{
		argv: argv, environment: environment, timeout: timeout,
		maxInputBytes: maxInput, maxStdoutBytes: maxStdout, maxStderrBytes: maxStderr,
		executable: executable,
	}, nil
}

// Review sends one valid JSON bundle through stdin and returns bounded raw
// stdout for the assessment validator. It creates and removes a fresh working
// directory for every invocation.
func (c *CLI) Review(ctx context.Context, input json.RawMessage) (Result, error) {
	if c == nil {
		return Result{}, errors.New("engine adapter is required")
	}
	if ctx == nil {
		return Result{}, errors.New("engine context is required")
	}
	if len(input) == 0 || len(input) > c.maxInputBytes || !json.Valid(input) {
		return Result{}, errors.New("engine input must be bounded valid JSON")
	}
	workdir, err := os.MkdirTemp("", "code-reviewer-engine-")
	if err != nil {
		return Result{}, fmt.Errorf("create engine work directory: %w", err)
	}
	defer os.RemoveAll(workdir)

	deadline, cancelDeadline := context.WithTimeout(ctx, c.timeout)
	defer cancelDeadline()
	runContext, cancelRun := context.WithCancel(deadline)
	defer cancelRun()

	stdout := newBoundedWriter(c.maxStdoutBytes, cancelRun)
	stderr := newBoundedWriter(c.maxStderrBytes, cancelRun)
	command := exec.CommandContext(runContext, c.executable, c.argv[1:]...)
	command.Args = append([]string{c.argv[0]}, c.argv[1:]...)
	command.Dir = workdir
	command.Env = c.commandEnvironment(workdir)
	command.Stdin = bytes.NewReader(input)
	command.Stdout = stdout
	command.Stderr = stderr
	configureCancellation(command)
	started := time.Now()
	err = command.Run()
	result := Result{
		Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Executable: c.executable,
		Duration: time.Since(started),
	}
	if stdout.Exceeded() || stderr.Exceeded() {
		return result, ErrOutputLimit
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if deadline.Err() != nil {
		return result, deadline.Err()
	}
	if err != nil {
		return result, fmt.Errorf("%w: %v", ErrEngineExit, err)
	}
	return result, nil
}

func (c *CLI) commandEnvironment(workdir string) []string {
	values := make(map[string]string, len(c.environment)+2)
	for key, value := range c.environment {
		values[key] = value
	}
	values["HOME"] = workdir
	values["TMPDIR"] = workdir
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

func validateEnvironment(configured map[string]string) (map[string]string, error) {
	allowed := map[string]struct{}{"LANG": {}, "LC_ALL": {}, "LC_CTYPE": {}, "PATH": {}}
	values := make(map[string]string, len(configured))
	for key, value := range configured {
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("engine environment variable %q is not allowed", key)
		}
		if strings.ContainsRune(value, 0) {
			return nil, fmt.Errorf("engine environment variable %q contains NUL", key)
		}
		values[key] = value
	}
	return values, nil
}

func normalizeTimeout(value time.Duration) (time.Duration, error) {
	if value == 0 {
		return DefaultTimeout, nil
	}
	if value < 0 || value > maxTimeout {
		return 0, fmt.Errorf("engine timeout must be positive and at most %s", maxTimeout)
	}
	return value, nil
}

func normalizeLimit(name string, value, fallback int) (int, error) {
	if value == 0 {
		return fallback, nil
	}
	if value < 0 {
		return 0, fmt.Errorf("%s limit must be positive", name)
	}
	return value, nil
}

type boundedWriter struct {
	mu       sync.Mutex
	limit    int
	bytes    []byte
	exceeded bool
	cancel   context.CancelFunc
}

func newBoundedWriter(limit int, cancel context.CancelFunc) *boundedWriter {
	return &boundedWriter{limit: limit, bytes: make([]byte, 0, limit), cancel: cancel}
}

func (w *boundedWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	remaining := w.limit - len(w.bytes)
	if remaining > 0 {
		if len(value) > remaining {
			w.bytes = append(w.bytes, value[:remaining]...)
		} else {
			w.bytes = append(w.bytes, value...)
		}
	}
	if len(value) > remaining {
		w.exceeded = true
		w.cancel()
	}
	return len(value), nil
}

func (w *boundedWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.bytes...)
}

func (w *boundedWriter) Exceeded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exceeded
}

var _ Adapter = (*CLI)(nil)
