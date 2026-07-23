//go:build darwin

package engine

import (
	"strings"
	"testing"
)

func TestMacOSSandboxProfileRestrictsAuthAndWrites(t *testing.T) {
	profile, err := macOSSandboxProfile("/opt/homebrew/bin/codex", "/Users/test/.codex", "/tmp/bridge")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"(deny default)", "(subpath \"/Users/test/.codex\")", "(subpath \"/tmp/bridge\")"} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile missing %q: %s", want, profile)
		}
	}
}

func TestMacOSSandboxCommandUsesProfile(t *testing.T) {
	command := macOSSandboxCommand("(version 1)", "/bin/echo", []string{"ok"})
	if command.Path != "/usr/bin/sandbox-exec" || strings.Join(command.Args, " ") != "/usr/bin/sandbox-exec -p (version 1) /bin/echo ok" {
		t.Fatalf("command = %#v", command.Args)
	}
}

func TestNewNativeFailsBeforeRunWhenAuthStateMissing(t *testing.T) {
	if _, err := NewNative(NativeConfig{Provider: ProviderCodex, Executable: "codex", AuthPath: "/missing-auth", BridgeRoot: t.TempDir()}); err == nil {
		t.Fatal("missing auth state accepted")
	}
}
