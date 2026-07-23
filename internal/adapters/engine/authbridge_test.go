package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateBridgeRunHomeIsPrivateAndRelative(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".reviewd", "engine-auth")
	home, err := CreateBridgeRunHome(root, "codex")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 || filepath.Dir(home) != root {
		t.Fatalf("bridge home = %s mode=%o", home, info.Mode().Perm())
	}
}
