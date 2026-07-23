package engine

import (
	"fmt"
	"os"
	"path/filepath"
)

// CreateBridgeRunHome creates one private, launch-relative provider bridge
// workspace. It never copies credentials; native adapters may add only their
// own narrowly scoped read-only authentication view in a later stage.
func CreateBridgeRunHome(root, provider string) (string, error) {
	if root == "" || provider == "" {
		return "", fmt.Errorf("bridge root and provider are required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve bridge root: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0o700); err != nil {
		return "", fmt.Errorf("create bridge root: %w", err)
	}
	if err := os.Chmod(absRoot, 0o700); err != nil {
		return "", fmt.Errorf("secure bridge root: %w", err)
	}
	home, err := os.MkdirTemp(absRoot, provider+"-")
	if err != nil {
		return "", fmt.Errorf("create bridge run home: %w", err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		_ = os.RemoveAll(home)
		return "", fmt.Errorf("secure bridge run home: %w", err)
	}
	return home, nil
}
