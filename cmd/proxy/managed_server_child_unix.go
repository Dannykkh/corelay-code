//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureManagedServerChildCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func isConnectionRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
