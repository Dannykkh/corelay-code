//go:build !windows && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

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
	return command.Process.Kill()
}

func platformProcessTreeKillSupported() bool {
	return false
}
