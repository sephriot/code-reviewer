package engine

import (
	"errors"
	"path/filepath"
	"strings"
)

// Provider identifies one supported native subscription CLI.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
	ProviderAgent  Provider = "agent"
)

// NativeConfig selects an authenticated provider CLI. AuthPath must be an
// existing provider-owned location; it is mounted read-only by the macOS
// sandbox profile and never copied into reviewd state.
type NativeConfig struct {
	Provider   Provider
	Executable string
	AuthPath   string
	BridgeRoot string
}

func (c NativeConfig) Validate() error {
	if c.Provider != ProviderClaude && c.Provider != ProviderCodex && c.Provider != ProviderAgent {
		return errors.New("native engine provider is invalid")
	}
	if strings.TrimSpace(c.Executable) == "" || strings.TrimSpace(c.AuthPath) == "" || strings.TrimSpace(c.BridgeRoot) == "" {
		return errors.New("native engine executable, auth path, and bridge root are required")
	}
	return nil
}

// Invocation is provider argv plus paths created inside one private bridge home.
type Invocation struct {
	Argv       []string
	SchemaPath string
	OutputPath string
}

func (c NativeConfig) Invocation(bridgeHome string) (Invocation, error) {
	if err := c.Validate(); err != nil {
		return Invocation{}, err
	}
	if strings.TrimSpace(bridgeHome) == "" {
		return Invocation{}, errors.New("bridge home is required")
	}
	schema := filepath.Join(bridgeHome, "assessment-schema.json")
	output := filepath.Join(bridgeHome, "assessment.json")
	switch c.Provider {
	case ProviderClaude:
		return Invocation{Argv: []string{c.Executable, "-p", "--output-format", "json", "--json-schema", schema}, SchemaPath: schema, OutputPath: output}, nil
	case ProviderCodex:
		return Invocation{Argv: []string{c.Executable, "exec", "--output-schema", schema, "--output-last-message", output, "-"}, SchemaPath: schema, OutputPath: output}, nil
	case ProviderAgent:
		return Invocation{Argv: []string{c.Executable, "--print", "--output-format", "json", "--mode", "ask"}, SchemaPath: schema, OutputPath: output}, nil
	default:
		return Invocation{}, errors.New("native engine provider is invalid")
	}
}
