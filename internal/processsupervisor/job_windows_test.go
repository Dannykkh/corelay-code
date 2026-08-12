//go:build windows

package processsupervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"golang.org/x/sys/windows"
)

const (
	streamingJobHelperEnabled = "CORELAY_STREAMING_JOB_HELPER"
	streamingJobHelperMode    = "CORELAY_STREAMING_JOB_MODE"
	streamingJobHelperReady   = "CORELAY_STREAMING_JOB_READY"
)

func TestWindowsJobRunnerStreamsAndReportsTruthfulCapabilities(t *testing.T) {
	t.Setenv("CORELAY_STREAMING_AMBIENT_SECRET", "must-not-cross")
	runner := newWindowsJobAdapter(AdapterDependencies{Lookup: exec.LookPath})
	capabilities := runner.Capabilities()
	if !capabilities.ProcessIsolation || !capabilities.ProcessTreeKill || !capabilities.ProcessLimits || !capabilities.MemoryLimits {
		t.Fatalf("capabilities=%#v", capabilities)
	}
	if capabilities.FilesystemIsolation || capabilities.NetworkIsolation || capabilities.EnvironmentIsolation {
		t.Fatalf("Windows Job Object overclaimed isolation: %#v", capabilities)
	}
	process, report := runner.Start(context.Background(), windowsJobPolicy(), windowsJobHelperSpec("inspect", t.TempDir()))
	if process == nil || !report.Started || process.PID() <= 0 || process.Command() != nil {
		t.Fatalf("process=%v report=%#v", process, report)
	}
	if _, err := io.WriteString(process.Stdin(), "stream-request\n"); err != nil {
		t.Fatal(err)
	}
	_ = process.Stdin().Close()
	var response struct {
		Request string `json:"request"`
		Secret  string `json:"secret"`
	}
	if err := json.NewDecoder(process.Stdout()).Decode(&response); err != nil {
		t.Fatalf("decode streaming response: %v", err)
	}
	if response.Request != "stream-request" || response.Secret != "" {
		t.Fatalf("response=%#v", response)
	}
	if err := process.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	diagnostic, err := io.ReadAll(process.Stderr())
	if err != nil || strings.TrimSpace(string(diagnostic)) != "diagnostic" {
		t.Fatalf("stderr=%q err=%v", diagnostic, err)
	}
}

func TestWindowsJobRunnerRejectsFilesystemAndNetworkPoliciesBeforeStart(t *testing.T) {
	runner := newWindowsJobAdapter(AdapterDependencies{Lookup: exec.LookPath})
	canary := filepath.Join(t.TempDir(), "payload-started")
	spec := windowsJobHelperSpec("canary", t.TempDir())
	spec.Environment.Set[streamingJobHelperReady] = canary
	for _, policy := range []sandbox.Policy{
		{Enforcement: sandbox.EnforcementRequired, Workspace: t.TempDir(), WorkspaceAccess: sandbox.WorkspaceReadOnly},
		{Enforcement: sandbox.EnforcementPreferred, Network: sandbox.NetworkDenied},
	} {
		process, report := runner.Start(context.Background(), policy, spec)
		if process != nil || report.Started || report.Failure != sandbox.FailureCapabilityUnavailable {
			t.Fatalf("policy=%#v process=%v report=%#v", policy, process, report)
		}
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("rejected payload ran: %v", err)
	}
}

func TestWindowsJobRunnerCancellationKillsDescendantTree(t *testing.T) {
	runner := newWindowsJobAdapter(AdapterDependencies{Lookup: exec.LookPath})
	ctx, cancel := context.WithCancel(context.Background())
	spec := windowsJobHelperSpec("parent", t.TempDir())
	ready := filepath.Join(t.TempDir(), "descendant-ready")
	spec.Environment.Set[streamingJobHelperReady] = ready
	process, report := runner.Start(ctx, windowsJobPolicy(), spec)
	if process == nil || !report.Started || !report.Applied.ProcessTreeKill {
		t.Fatalf("process=%v report=%#v", process, report)
	}
	var identity windowsStreamingChildIdentity
	if err := json.NewDecoder(process.Stdout()).Decode(&identity); err != nil {
		t.Fatalf("decode child identity: %v", err)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("descendant readiness barrier: %v", err)
	}
	cancel()
	select {
	case <-process.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("canceled Job Object process was not reaped")
	}
	assertWindowsProcessIdentityExited(t, identity)
}

func TestWindowsStreamingJobHelper(t *testing.T) {
	if os.Getenv(streamingJobHelperEnabled) != "1" {
		return
	}
	switch os.Getenv(streamingJobHelperMode) {
	case "inspect":
		request, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{
			"request": strings.TrimSpace(request), "secret": os.Getenv("CORELAY_STREAMING_AMBIENT_SECRET"),
		})
		_, _ = io.WriteString(os.Stderr, "diagnostic\n")
		os.Exit(0)
	case "canary":
		_ = os.WriteFile(os.Getenv(streamingJobHelperReady), []byte("started"), 0o600)
		os.Exit(0)
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestWindowsStreamingJobHelper$")
		child.Env = replaceWindowsTestEnvironment(os.Environ(), streamingJobHelperMode, "child")
		child.Stdout, child.Stderr = io.Discard, io.Discard
		if err := child.Start(); err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(21)
		}
		if err := waitForWindowsTestFile(os.Getenv(streamingJobHelperReady), 5*time.Second); err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(22)
		}
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(child.Process.Pid))
		if err != nil {
			os.Exit(23)
		}
		created, err := windowsProcessCreationTime(handle)
		_ = windows.CloseHandle(handle)
		if err != nil {
			os.Exit(24)
		}
		_ = json.NewEncoder(os.Stdout).Encode(windowsStreamingChildIdentity{PID: child.Process.Pid, Created: created})
		time.Sleep(time.Hour)
	case "child":
		if err := os.WriteFile(os.Getenv(streamingJobHelperReady), []byte("ready"), 0o600); err != nil {
			os.Exit(25)
		}
		time.Sleep(time.Hour)
	default:
		os.Exit(26)
	}
}

type windowsStreamingChildIdentity struct {
	PID     int    `json:"pid"`
	Created uint64 `json:"created"`
}

func windowsJobPolicy() sandbox.Policy {
	return sandbox.Policy{
		Enforcement: sandbox.EnforcementRequired,
		Required: sandbox.Capabilities{
			ProcessIsolation: true, ProcessTreeKill: true, EnvironmentFiltering: true, Timeouts: true,
		},
		MaxProcesses: 4, MemoryLimitBytes: 512 * 1024 * 1024,
	}
}

func windowsJobHelperSpec(mode, directory string) Spec {
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		executable = os.Args[0]
	}
	return Spec{
		Executable: executable, Args: []string{"-test.run=^TestWindowsStreamingJobHelper$"}, Dir: directory,
		Environment: sandbox.EnvironmentSpec{
			Inherit: []string{"PATH", "PATHEXT", "SystemRoot", "WINDIR", "COMSPEC", "TEMP", "TMP"},
			Set:     map[string]string{streamingJobHelperEnabled: "1", streamingJobHelperMode: mode},
		},
	}
}

func assertWindowsProcessIdentityExited(t *testing.T, identity windowsStreamingChildIdentity) {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(identity.PID))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return
	}
	if err != nil {
		t.Fatalf("open descendant %d: %v", identity.PID, err)
	}
	defer windows.CloseHandle(handle)
	created, err := windowsProcessCreationTime(handle)
	if err != nil {
		t.Fatal(err)
	}
	if created != identity.Created {
		t.Logf("descendant PID %d was reused", identity.PID)
		return
	}
	wait, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || wait != windows.WAIT_OBJECT_0 {
		t.Fatalf("descendant %d survived Job Object cancellation: wait=%d err=%v", identity.PID, wait, err)
	}
}

func windowsProcessCreationTime(process windows.Handle) (uint64, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(process, &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	return uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime), nil
}

func waitForWindowsTestFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %q", path)
}

func replaceWindowsTestEnvironment(environment []string, name, value string) []string {
	prefix := strings.ToUpper(name) + "="
	result := make([]string, 0, len(environment)+1)
	replaced := false
	for _, entry := range environment {
		if strings.HasPrefix(strings.ToUpper(entry), prefix) {
			result = append(result, name+"="+value)
			replaced = true
		} else {
			result = append(result, entry)
		}
	}
	if !replaced {
		result = append(result, name+"="+value)
	}
	return result
}
