//go:build !windows

package main

import (
	"os/exec"
	"testing"
)

func TestManagedServerChildUsesSeparateProcessGroup(t *testing.T) {
	cmd := exec.Command("unused")
	configureManagedServerChildCommand(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("managed server child must be isolated from the CLI process group")
	}
}
