//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package engine

import "os/exec"

// configureCancellation provides direct cancellation where no supported
// native process-group implementation is available.
func configureCancellation(command *exec.Cmd) {
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return command.Process.Kill()
	}
}
