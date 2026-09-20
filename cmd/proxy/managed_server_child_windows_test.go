//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestManagedServerChildUsesSeparateConsoleProcessGroup(t *testing.T) {
	cmd := exec.Command("unused")
	configureManagedServerChildCommand(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Fatal("managed server child must remain headless")
	}
	if cmd.SysProcAttr.CreationFlags&syscall.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatal("managed server child must be isolated from the CLI console Ctrl-C group")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatal("managed server child must not create a console window")
	}
}
