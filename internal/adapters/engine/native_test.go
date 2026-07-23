package engine

import "testing"

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
