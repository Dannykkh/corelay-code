//go:build !windows && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package processsupervisor

import (
	"os"
	"os/exec"
)

func configureProcess(_ *exec.Cmd) {}

func terminateProcess(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	return command.Process.Kill()
}

func processTreeKillSupported() bool { return false }
