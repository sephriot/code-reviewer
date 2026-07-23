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
