package engine

import (
	"slices"
	"testing"
)

func TestNativeConfigValidation(t *testing.T) {
	valid := NativeConfig{Provider: ProviderCodex, Executable: "codex", AuthPath: "/provider/auth", BridgeRoot: ".reviewd/engine-auth"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	valid.Provider = "unknown"
	if err := valid.Validate(); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestAgentInvocationTrustsPrivateBridge(t *testing.T) {
	invocation, err := (NativeConfig{Provider: ProviderAgent, Executable: "agent", AuthPath: "/auth", BridgeRoot: ".reviewd"}).Invocation("/tmp/bridge")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(invocation.Argv, "--trust") {
		t.Fatalf("agent argv missing --trust: %q", invocation.Argv)
	}
}

func TestNativeInvocationUsesStructuredModes(t *testing.T) {
	for _, provider := range []Provider{ProviderClaude, ProviderCodex, ProviderAgent} {
		invocation, err := (NativeConfig{Provider: provider, Executable: string(provider), AuthPath: "/auth", BridgeRoot: ".reviewd"}).Invocation("/tmp/bridge")
		if err != nil || invocation.SchemaPath != "/tmp/bridge/assessment-schema.json" || len(invocation.Argv) == 0 {
			t.Fatalf("provider=%s invocation=%+v err=%v", provider, invocation, err)
		}
	}
}

func TestClaudeInvocationReservesSchemaArgument(t *testing.T) {
	invocation, err := (NativeConfig{Provider: ProviderClaude, Executable: "claude", AuthPath: "/auth", BridgeRoot: ".reviewd"}).Invocation("/tmp/bridge")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := invocation.Argv[len(invocation.Argv)-1], "/tmp/bridge/assessment-schema.json"; got != want {
		t.Fatalf("schema placeholder=%q want=%q", got, want)
	}
}
