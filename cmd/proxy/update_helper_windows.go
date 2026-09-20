//go:build windows

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func shouldDeferSelfUpdate(target string) bool {
	current, err := os.Executable()
	if err != nil {
		return false
	}
	return sameWindowsPath(current, target)
}

func sameWindowsPath(left, right string) bool {
	left, leftErr := filepath.Abs(left)
	right, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(left); err == nil {
		left = resolved
	}
	if resolved, err := filepath.EvalSymlinks(right); err == nil {
		right = resolved
	}
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func scheduleSelfUpdate(request updateHelperRequest) (string, error) {
	if request.ParentPID == 0 || strings.TrimSpace(request.Target) == "" {
		return "", fmt.Errorf("parent process and target are required")
	}
	if !request.Rollback && (strings.TrimSpace(request.Artifact) == "" || strings.TrimSpace(request.Digest) == "") {
		return "", fmt.Errorf("artifact and digest are required")
	}
	current, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}
	helperPath, err := copyUpdateHelper(current)
	if err != nil {
		return "", err
	}
	args := []string{
		"update-helper",
		"-target", request.Target,
		"-parent-pid", updateHelperParentArgument(request.ParentPID),
		"-helper-path", helperPath,
	}
	if request.Rollback {
		args = append(args, "-rollback")
	} else {
		args = append(args, "-artifact", request.Artifact, "-sha256", request.Digest)
	}
	if strings.TrimSpace(request.Backup) != "" {
		args = append(args, "-backup", request.Backup)
	}
	command := exec.Command(helperPath, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	configureUpdateHelperCommand(command)
	if err := command.Start(); err != nil {
		_ = os.Remove(helperPath)
		return "", fmt.Errorf("start update helper: %w", err)
	}
	return helperPath, nil
}

func copyUpdateHelper(source string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open current executable: %w", err)
	}
	defer input.Close()
	file, err := os.CreateTemp(os.TempDir(), ".corelay-update-helper-*.exe")
	if err != nil {
		return "", fmt.Errorf("create update helper: %w", err)
	}
	helperPath := file.Name()
	removeOnError := true
	defer func() {
		_ = file.Close()
		if removeOnError {
			_ = os.Remove(helperPath)
		}
	}()
	if err := file.Chmod(0o700); err != nil {
		return "", fmt.Errorf("set update helper mode: %w", err)
	}
	if _, err := io.Copy(file, input); err != nil {
		return "", fmt.Errorf("copy update helper: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("flush update helper: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close update helper: %w", err)
	}
	removeOnError = false
	return helperPath, nil
}

func configureUpdateHelperCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
	}
}

func waitForUpdateParent(pid uint32) error {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		if err == windows.ERROR_INVALID_PARAMETER {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(handle)
	event, err := windows.WaitForSingleObject(handle, windows.INFINITE)
	if err != nil {
		return err
	}
	if event != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("parent wait returned %d", event)
	}
	return nil
}

func validateUpdateHelperPath(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("invalid helper path: %w", err)
	}
	base := filepath.Base(absolute)
	if !strings.HasPrefix(base, ".corelay-update-helper-") || !strings.HasSuffix(strings.ToLower(base), ".exe") {
		return fmt.Errorf("helper path is not an updater temporary executable")
	}
	tempRoot, err := filepath.Abs(os.TempDir())
	if err != nil {
		return fmt.Errorf("resolve temporary directory: %w", err)
	}
	relative, err := filepath.Rel(tempRoot, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("helper path is outside the temporary directory")
	}
	if strings.ContainsAny(absolute, "\"&|<>\r\n") {
		return fmt.Errorf("helper path contains unsafe shell characters")
	}
	return nil
}

func cleanupUpdateHelper(path string) {
	if validateUpdateHelperPath(path) != nil {
		return
	}
	commandLine := "ping -n 2 127.0.0.1 >nul & del /f /q \"" + strings.ReplaceAll(path, "\"", "\"\"") + "\""
	shell := os.Getenv("ComSpec")
	if strings.TrimSpace(shell) == "" {
		shell = "cmd.exe"
	}
	command := exec.Command(shell, "/d", "/c", commandLine)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	configureUpdateHelperCommand(command)
	_ = command.Start()
}
