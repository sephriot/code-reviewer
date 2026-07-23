//go:build darwin

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// Native executes one supported subscription CLI in a macOS sandbox.
type Native struct{ config NativeConfig }

func NewNative(config NativeConfig) (*Native, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath(config.Executable); err != nil {
		return nil, fmt.Errorf("resolve native engine executable: %w", err)
	}
	if _, err := os.Stat(config.AuthPath); err != nil {
		return nil, fmt.Errorf("read native engine auth state: %w", err)
	}
	return &Native{config: config}, nil
}

func (n *Native) Review(ctx context.Context, input json.RawMessage) (Result, error) {
	if n == nil || !json.Valid(input) {
		return Result{}, fmt.Errorf("native engine and valid input are required")
	}
	home, err := CreateBridgeRunHome(n.config.BridgeRoot, string(n.config.Provider))
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(home)
	invocation, err := n.config.Invocation(home)
	if err != nil {
		return Result{}, err
	}
	if err := WriteAssessmentSchema(invocation.SchemaPath); err != nil {
		return Result{}, err
	}
	profile, err := macOSSandboxProfile(invocation.Argv[0], n.config.AuthPath, home)
	if err != nil {
		return Result{}, err
	}
	command := macOSSandboxCommand(profile, invocation.Argv[0], invocation.Argv[1:])
	command.Dir = home
	command.Env = []string{"HOME=" + home, "TMPDIR=" + home, "PATH=/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin", "CODEX_HOME=" + n.config.AuthPath}
	command.Stdin = bytes.NewReader(input)
	output, err := command.Output()
	if err != nil {
		return Result{Stdout: output, Executable: invocation.Argv[0]}, fmt.Errorf("native engine execution: %w", err)
	}
	if n.config.Provider == ProviderCodex {
		if raw, readErr := os.ReadFile(invocation.OutputPath); readErr == nil {
			output = raw
		}
	}
	normalized, err := NormalizeNativeOutput(n.config.Provider, output)
	if err != nil {
		return Result{Stdout: output, Executable: invocation.Argv[0]}, err
	}
	return Result{Stdout: normalized, Executable: invocation.Argv[0]}, nil
}
