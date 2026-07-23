package engine

import (
	"errors"
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
