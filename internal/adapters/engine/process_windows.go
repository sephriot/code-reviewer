//go:build windows

package engine

import "os/exec"

// configureCancellation terminates the direct process on Windows. Native
// restricted/container isolation owns any stronger process-tree boundary.
func configureCancellation(command *exec.Cmd) {
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return command.Process.Kill()
	}
}
