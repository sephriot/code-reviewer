//go:build darwin

package engine

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// macOSSandboxProfile permits provider authentication reads and bridge writes
// only. Native adapters supply absolute provider paths; caller never embeds
// credentials in the profile.
func macOSSandboxProfile(executable, authPath, bridgeHome string) (string, error) {
	for _, value := range []string{executable, authPath, bridgeHome} {
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("sandbox path must be absolute")
		}
	}
	quote := func(value string) string { return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"` }
	return "(version 1)\n(deny default)\n" +
		"(allow process*)\n(allow network-outbound)\n" +
		"(allow file-read* (subpath \"/System\") (subpath \"/usr\") (subpath \"/opt/homebrew\") (subpath " + quote(authPath) + ") (literal " + quote(executable) + "))\n" +
		"(allow file-write* (subpath " + quote(bridgeHome) + "))\n", nil
}

func macOSSandboxCommand(profile, executable string, argv []string) *exec.Cmd {
	arguments := append([]string{"-p", profile, executable}, argv...)
	return exec.Command("/usr/bin/sandbox-exec", arguments...)
}
