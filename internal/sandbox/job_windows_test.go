//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	windowsJobHelperEnabled = "CORELAY_WINDOWS_JOB_HELPER"
	windowsJobHelperMode    = "CORELAY_WINDOWS_JOB_HELPER_MODE"
	windowsJobHelperReady   = "CORELAY_WINDOWS_JOB_HELPER_READY"
	windowsJobHelperStart   = "CORELAY_WINDOWS_JOB_HELPER_START"
)

func TestWindowsJobRunnerCapabilitiesAndPolicyRejection(t *testing.T) {
	runner := newWindowsJobAdapter(AdapterDependencies{Lookup: exec.LookPath})
	capabilities := runner.Capabilities()
	if !capabilities.ProcessIsolation || !capabilities.ProcessLimits || !capabilities.MemoryLimits || !capabilities.ProcessTreeKill {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	if capabilities.FilesystemIsolation || capabilities.NetworkIsolation {
		t.Fatalf("Windows Job Object overclaimed capabilities: %#v", capabilities)
	}
	for _, policy := range []Policy{
		{Enforcement: EnforcementRequired, Required: Capabilities{FilesystemIsolation: true}},
		{Enforcement: EnforcementRequired, Network: NetworkDenied},
	} {
		result, report := runner.Run(context.Background(), policy, windowsJobHelperCommand("child"))
		if result.Started || report.Failure != FailureCapabilityUnavailable {
			t.Fatalf("policy=%#v result=%#v report=%#v", policy, result, report)
		}
	}
}

func TestWindowsJobRunnerKillsDescendantTreeOnTimeout(t *testing.T) {
	runner := newWindowsJobAdapter(AdapterDependencies{Lookup: exec.LookPath})
	command := windowsJobHelperCommand("parent")
	readyPath := filepath.Join(t.TempDir(), "descendant-ready")
	command.Environment.Set[windowsJobHelperReady] = readyPath
	command.Timeout = 1500 * time.Millisecond
	result, report := runner.Run(context.Background(), Policy{
		Enforcement:      EnforcementRequired,
		Required:         Capabilities{ProcessIsolation: true, ProcessTreeKill: true},
		MaxProcesses:     4,
		MemoryLimitBytes: 512 * 1024 * 1024,
	}, command)
	if !result.Started || !result.TimedOut || report.Failure != FailureTimedOut {
		t.Fatalf("result=%#v report=%#v stdout=%q stderr=%q", result, report, result.Stdout, result.Stderr)
	}
	if !report.AppliedIsolation.ProcessTreeKill {
		t.Fatalf("process-tree termination was not verified: report=%#v", report)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("descendant did not reach ready barrier: %v", err)
	}
	child, err := parseChildIdentity(string(result.Stdout))
	if err != nil {
		t.Fatal(err)
	}
	process, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(child.pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		// A process that has already left the process table is the expected result.
		return
	}
	if err != nil {
		t.Fatalf("open descendant process %d: %v", child.pid, err)
	}
	defer windows.CloseHandle(process)
	creationTime, err := windowsProcessCreationTime(process)
	if err != nil {
		t.Fatalf("read child process identity: %v", err)
	}
	if creationTime != child.creationTime {
		// The original descendant exited and Windows reused its PID before this
		// observation. Do not mistake the unrelated process for a survivor.
		t.Logf("descendant PID %d was reused after verified Job Object drain", child.pid)
		return
	}
	wait, err := windows.WaitForSingleObject(process, 0)
	if err != nil {
		t.Fatalf("check child process: %v", err)
	}
	if wait != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant process %d survived Job Object termination", child.pid)
	}
}

func TestWindowsJobRunnerEnforcesCombinedOutputLimit(t *testing.T) {
	runner := newWindowsJobAdapter(AdapterDependencies{Lookup: exec.LookPath})
	command := outputHelperCommand(2048, 2048, true, 0)
	command.OutputLimitBytes = 1000
	command.Timeout = 5 * time.Second
	result, report := runner.Run(context.Background(), Policy{
		Enforcement: EnforcementRequired,
		Required:    Capabilities{ProcessIsolation: true, ProcessTreeKill: true},
	}, command)
	assertOutputLimitFailure(t, result, report, command.OutputLimitBytes)
	if !report.AppliedIsolation.ProcessTreeKill {
		t.Fatalf("output termination was not verified: %#v", report)
	}
}

func TestWindowsJobRunnerKillsDescendantWriterOnOutputLimit(t *testing.T) {
	runner := newWindowsJobAdapter(AdapterDependencies{Lookup: exec.LookPath})
	command := windowsJobHelperCommand("output-parent")
	readyPath := filepath.Join(t.TempDir(), "descendant-ready")
	startPath := filepath.Join(t.TempDir(), "descendant-start")
	command.Environment.Set[windowsJobHelperReady] = readyPath
	command.Environment.Set[windowsJobHelperStart] = startPath
	command.OutputLimitBytes = 1024
	command.Timeout = 5 * time.Second
	result, report := runner.Run(context.Background(), Policy{
		Enforcement:  EnforcementRequired,
		Required:     Capabilities{ProcessIsolation: true, ProcessTreeKill: true},
		MaxProcesses: 4,
	}, command)
	assertOutputLimitFailure(t, result, report, command.OutputLimitBytes)
	if !report.AppliedIsolation.ProcessTreeKill {
		t.Fatalf("descendant writer termination was not verified: %#v", report)
	}
	child, err := parseChildIdentity(string(result.Stdout))
	if err != nil {
		t.Fatal(err)
	}
	process, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(child.pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return
	}
	if err != nil {
		t.Fatalf("open descendant writer %d: %v", child.pid, err)
	}
	defer windows.CloseHandle(process)
	creationTime, err := windowsProcessCreationTime(process)
	if err != nil {
		t.Fatalf("read descendant writer identity: %v", err)
	}
	if creationTime != child.creationTime {
		return
	}
	wait, err := windows.WaitForSingleObject(process, 0)
	if err != nil {
		t.Fatalf("check descendant writer: %v", err)
	}
	if wait != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant writer %d survived output-limit termination", child.pid)
	}
}

func TestWindowsJobRunnerTimeoutOutputLimitRaceHasOneTerminalReason(t *testing.T) {
	runner := newWindowsJobAdapter(AdapterDependencies{Lookup: exec.LookPath})
	startedRuns := 0
	for iteration := 0; iteration < 6; iteration++ {
		command := outputHelperCommand(4096, 0, false, 75*time.Millisecond)
		command.OutputLimitBytes = 32
		command.Timeout = 75 * time.Millisecond
		result, report := runner.Run(context.Background(), Policy{
			Enforcement: EnforcementRequired,
			Required:    Capabilities{ProcessIsolation: true, ProcessTreeKill: true},
		}, command)
		if !result.Started {
			// The command timeout covers sandbox setup as well as execution. A
			// cold or heavily loaded Windows runner may therefore exhaust this
			// deliberately short race budget before the suspended process is
			// assigned and resumed. That is a valid, fail-closed timeout, not a
			// violation of the single-terminal-reason contract.
			if !result.TimedOut || result.Canceled || result.OutputTruncated ||
				report.Started || report.Failure != FailureTimedOut ||
				report.AppliedIsolation != (Capabilities{}) {
				t.Fatalf("pre-start timeout was not exclusive: iteration=%d result=%#v report=%#v", iteration, result, report)
			}
			continue
		}
		startedRuns++
		if result.Canceled || !report.Started || !report.AppliedIsolation.ProcessTreeKill {
			t.Fatalf("iteration=%d result=%#v report=%#v", iteration, result, report)
		}
		switch report.Failure {
		case FailureOutputLimit:
			if result.TimedOut || !result.OutputTruncated {
				t.Fatalf("output limit was not exclusive: iteration=%d result=%#v report=%#v", iteration, result, report)
			}
		case FailureTimedOut:
			if !result.TimedOut {
				t.Fatalf("timeout was not exclusive: iteration=%d result=%#v report=%#v", iteration, result, report)
			}
		default:
			t.Fatalf("iteration=%d failure=%q result=%#v report=%#v", iteration, report.Failure, result, report)
		}
	}
	if startedRuns == 0 {
		t.Fatal("all race attempts exhausted their deadlines during sandbox setup")
	}
}

func TestWindowsJobRunnerFailsClosedWhenTerminationVerificationFails(t *testing.T) {
	tests := []struct {
		name       string
		snapshot   windowsJobProcessSnapshotter
		active     windowsJobProcessCounter
		wantDetail string
	}{
		{
			name: "process snapshot access denied",
			snapshot: func(windows.Handle) ([]windowsJobProcessHandle, error) {
				return nil, windows.ERROR_ACCESS_DENIED
			},
			active:     windowsJobActiveProcessCount,
			wantDetail: "snapshot Job Object processes",
		},
		{
			name:     "active process query access denied",
			snapshot: snapshotWindowsJobProcesses,
			active: func(windows.Handle) (uint32, error) {
				return 0, windows.ERROR_ACCESS_DENIED
			},
			wantDetail: "query active process count",
		},
		{
			name:     "active process drain exceeds grace",
			snapshot: snapshotWindowsJobProcesses,
			active: func(windows.Handle) (uint32, error) {
				return 1, nil
			},
			wantDetail: "timed out waiting for 1 active process",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const terminationGrace = 100 * time.Millisecond
			runner := &WindowsJobRunner{
				lookup:               exec.LookPath,
				snapshotProcesses:    test.snapshot,
				activeProcessCount:   test.active,
				terminationGraceTime: terminationGrace,
			}
			command := windowsJobHelperCommand("child")
			command.Environment.Set[windowsJobHelperReady] = filepath.Join(t.TempDir(), "root-ready")
			command.Timeout = 300 * time.Millisecond
			startedAt := time.Now()
			result, report := runner.Run(context.Background(), Policy{
				Enforcement: EnforcementRequired,
				Required:    Capabilities{ProcessIsolation: true, ProcessTreeKill: true},
			}, command)
			elapsed := time.Since(startedAt)
			if !result.Started || !result.TimedOut || report.Failure != FailureExecutionFailed {
				t.Fatalf("result=%#v report=%#v", result, report)
			}
			if report.AppliedIsolation.ProcessTreeKill {
				t.Fatalf("failed termination verification overclaimed process-tree kill: %#v", report)
			}
			if !runner.Capabilities().ProcessTreeKill {
				t.Fatal("runtime verification failure incorrectly downgraded adapter capability")
			}
			if !strings.Contains(report.Detail, test.wantDetail) {
				t.Fatalf("detail=%q, want substring %q", report.Detail, test.wantDetail)
			}
			if maximum := command.Timeout + terminationGrace + time.Second; elapsed > maximum {
				t.Fatalf("run exceeded execution plus termination budgets: elapsed=%v maximum=%v", elapsed, maximum)
			}
		})
	}
}

func TestWaitForWindowsJobEmptyHonorsSharedDeadline(t *testing.T) {
	const budget = 25 * time.Millisecond
	startedAt := time.Now()
	err := waitForWindowsJobEmpty(0, startedAt.Add(budget), func(windows.Handle) (uint32, error) {
		return 1, nil
	})
	if err == nil {
		t.Fatal("wait unexpectedly succeeded for permanently active Job Object")
	}
	if elapsed := time.Since(startedAt); elapsed > budget+250*time.Millisecond {
		t.Fatalf("drain exceeded shared deadline: elapsed=%v budget=%v", elapsed, budget)
	}
}

func TestWindowsJobHelperProcess(t *testing.T) {
	if os.Getenv(windowsJobHelperEnabled) != "1" {
		return
	}
	switch os.Getenv(windowsJobHelperMode) {
	case "parent", "output-parent":
		outputWriter := os.Getenv(windowsJobHelperMode) == "output-parent"
		childMode := "child"
		if outputWriter {
			childMode = "output-child"
		}
		child := exec.Command(os.Args[0], "-test.run=^TestWindowsJobHelperProcess$")
		child.Env = replaceEnvironmentValue(os.Environ(), windowsJobHelperMode, childMode)
		if outputWriter {
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
		} else {
			child.Stdout = io.Discard
			child.Stderr = io.Discard
		}
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "start descendant: %v", err)
			os.Exit(21)
		}
		if err := waitForWindowsJobHelperReady(os.Getenv(windowsJobHelperReady), 10*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "wait for descendant readiness: %v", err)
			os.Exit(23)
		}
		process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(child.Process.Pid))
		if err != nil {
			fmt.Fprintf(os.Stderr, "open descendant: %v", err)
			os.Exit(24)
		}
		creationTime, err := windowsProcessCreationTime(process)
		windows.CloseHandle(process)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read descendant identity: %v", err)
			os.Exit(25)
		}
		fmt.Fprintf(os.Stdout, "child-pid=%d child-created=%d\n", child.Process.Pid, creationTime)
		if outputWriter {
			if err := os.WriteFile(os.Getenv(windowsJobHelperStart), []byte("start"), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "release descendant writer: %v", err)
				os.Exit(27)
			}
		}
		time.Sleep(30 * time.Second)
	case "child":
		if err := os.WriteFile(os.Getenv(windowsJobHelperReady), []byte("ready"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "signal descendant readiness: %v", err)
			os.Exit(26)
		}
		time.Sleep(30 * time.Second)
	case "output-child":
		if err := os.WriteFile(os.Getenv(windowsJobHelperReady), []byte("ready"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "signal descendant writer readiness: %v", err)
			os.Exit(28)
		}
		if err := waitForWindowsJobHelperReady(os.Getenv(windowsJobHelperStart), 10*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "wait for descendant writer release: %v", err)
			os.Exit(29)
		}
		payload := strings.Repeat("X", 4096)
		for {
			if _, err := io.WriteString(os.Stdout, payload); err != nil {
				os.Exit(0)
			}
		}
	default:
		os.Exit(22)
	}
}

func windowsJobHelperCommand(mode string) CommandSpec {
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		executable = os.Args[0]
	}
	return CommandSpec{
		Path: executable,
		Args: []string{"-test.run=^TestWindowsJobHelperProcess$"},
		Environment: EnvironmentSpec{
			Inherit: essentialEnvironmentNames(),
			Set: map[string]string{
				windowsJobHelperEnabled: "1",
				windowsJobHelperMode:    mode,
			},
		},
	}
}

type windowsChildIdentity struct {
	pid          int
	creationTime uint64
}

func parseChildIdentity(output string) (windowsChildIdentity, error) {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "child-pid=") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				break
			}
			pid, pidErr := strconv.Atoi(strings.TrimPrefix(fields[0], "child-pid="))
			creationTime, creationErr := strconv.ParseUint(strings.TrimPrefix(fields[1], "child-created="), 10, 64)
			if pidErr != nil || creationErr != nil || !strings.HasPrefix(fields[1], "child-created=") {
				break
			}
			return windowsChildIdentity{pid: pid, creationTime: creationTime}, nil
		}
	}
	return windowsChildIdentity{}, fmt.Errorf("child identity missing from output %q", output)
}

func windowsProcessCreationTime(process windows.Handle) (uint64, error) {
	var creationTime windows.Filetime
	var exitTime windows.Filetime
	var kernelTime windows.Filetime
	var userTime windows.Filetime
	if err := windows.GetProcessTimes(process, &creationTime, &exitTime, &kernelTime, &userTime); err != nil {
		return 0, err
	}
	return uint64(creationTime.HighDateTime)<<32 | uint64(creationTime.LowDateTime), nil
}

func waitForWindowsJobHelperReady(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %q", path)
		}
		time.Sleep(time.Millisecond)
	}
}

func replaceEnvironmentValue(environment []string, name, value string) []string {
	prefix := strings.ToUpper(name) + "="
	replaced := false
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if strings.HasPrefix(strings.ToUpper(item), prefix) {
			result = append(result, name+"="+value)
			replaced = true
		} else {
			result = append(result, item)
		}
	}
	if !replaced {
		result = append(result, name+"="+value)
	}
	return result
}
