//go:build windows

package sandbox

import (
	"os"
	"os/exec"
)

func configureProcessTree(_ *exec.Cmd) {}

func terminateProcessTree(command *exec.Cmd) error {
	if command.Process == nil {
		return os.ErrProcessDone
	}
	// The Go standard library exposes termination of the direct process on
	// Windows, not a truthful job-object process-tree guarantee.
	return command.Process.Kill()
}

func platformProcessTreeKillSupported() bool {
	return false
}
