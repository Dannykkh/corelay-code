package tray

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type trayFakeRunner struct {
	commands []sandbox.CommandSpec
	fail     bool
}

func (*trayFakeRunner) Name() string { return "tray-fake" }
func (*trayFakeRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{ProcessIsolation: true, ProcessTreeKill: true, EnvironmentFiltering: true, Timeouts: true}
}
func (r *trayFakeRunner) Run(_ context.Context, _ sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
	r.commands = append(r.commands, command)
	if r.fail {
		return sandbox.Result{ExitCode: sandbox.ExitNotStarted, Err: errors.New("raw secret")}, sandbox.Report{Failure: sandbox.FailureStartFailed, Detail: "raw secret"}
	}
	return sandbox.Result{Started: true, ExitCode: 0}, sandbox.Report{Runner: r.Name(), Started: true}
}

func TestNotificationCommandsNeverInterpolatePayloadIntoScripts(t *testing.T) {
	title := `title"; Start-Process calc; #`
	message := `message" & rm -rf ignored`
	windowsCommand, err := notificationCommand("windows", title, message)
	if err != nil {
		t.Fatal(err)
	}
	script := windowsCommand.Args[len(windowsCommand.Args)-1]
	if strings.Contains(script, title) || strings.Contains(script, message) {
		t.Fatalf("payload was interpolated into PowerShell: %q", script)
	}
	if windowsCommand.Environment.Set["CORELAY_NOTIFICATION_TITLE"] != title || windowsCommand.Environment.Set["CORELAY_NOTIFICATION_MESSAGE"] != message {
		t.Fatalf("Windows payload environment = %#v", windowsCommand.Environment.Set)
	}
	if _, ok := windowsCommand.Environment.Set["ANICLEW_NOTIFICATION_TITLE"]; ok {
		t.Fatalf("legacy Windows payload environment should only be a script fallback: %#v", windowsCommand.Environment.Set)
	}
	if !strings.Contains(script, "ANICLEW_NOTIFICATION_TITLE") || !strings.Contains(script, "ANICLEW_NOTIFICATION_MESSAGE") {
		t.Fatalf("legacy Windows payload fallback missing from PowerShell: %q", script)
	}

	macCommand, err := notificationCommand("darwin", title, message)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(macCommand.Args[1], title) || strings.Contains(macCommand.Args[1], message) || macCommand.Args[len(macCommand.Args)-2] != title || macCommand.Args[len(macCommand.Args)-1] != message {
		t.Fatalf("macOS argv = %#v", macCommand.Args)
	}
}

func TestTrayHostActionsUseRunnerAndValidatePort(t *testing.T) {
	runner := &trayFakeRunner{}
	options := HostExecutionOptions{
		Context: context.Background(), Runner: runner,
		Policy:  sandbox.Policy{Enforcement: sandbox.EnforcementPreferred},
		Timeout: time.Second,
	}
	if err := ShowNotificationWithOptions("title", "message", options); err != nil {
		t.Fatal(err)
	}
	if err := OpenBrowserWithOptions(0, options); err == nil {
		t.Fatal("invalid port was accepted")
	}
	if err := OpenBrowserWithOptions(65536, options); err == nil {
		t.Fatal("oversized port was accepted")
	}
	if err := OpenBrowserWithOptions(8080, options); err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("commands = %d", len(runner.commands))
	}
	browser := runner.commands[1]
	if got := browser.Args[len(browser.Args)-1]; got != "http://localhost:8080/app" {
		t.Fatalf("browser URL = %q", got)
	}
}

func TestTrayFailureDoesNotExposeRunnerErrorOrPayload(t *testing.T) {
	runner := &trayFakeRunner{fail: true}
	err := ShowNotificationWithOptions("private title", "private message", HostExecutionOptions{
		Context: context.Background(), Runner: runner,
		Policy:  sandbox.Policy{Enforcement: sandbox.EnforcementPreferred},
		Timeout: time.Second,
	})
	if err == nil || strings.Contains(err.Error(), "raw secret") || strings.Contains(err.Error(), "private") {
		t.Fatalf("error = %v", err)
	}
}

func TestTrayZeroExecutionOptionsFailClosed(t *testing.T) {
	if err := ShowNotificationWithOptions("title", "message", HostExecutionOptions{}); err == nil {
		t.Fatal("zero execution options were accepted")
	}
}
