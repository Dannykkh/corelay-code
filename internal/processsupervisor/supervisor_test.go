package processsupervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func TestHostRunnerPreservesArgvCanonicalizesDirAndFiltersEnvironment(t *testing.T) {
	workingDir := t.TempDir()
	if err := os.Setenv("MCP_SUPERVISOR_AMBIENT_SECRET", "must-not-cross"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv("MCP_SUPERVISOR_AMBIENT_SECRET") })
	args := []string{"-test.run=^TestProcessSupervisorHelper$", "--", "semi;colon", "space value"}
	environment := sandbox.EnvironmentSpec{Set: map[string]string{"MCP_SUPERVISOR_HELPER": "inspect"}}

	process, report := NewHostRunner().Start(context.Background(), disabledTestPolicy(), Spec{
		Executable: os.Args[0], Args: args, Dir: workingDir, Environment: environment,
	})
	args[2] = "mutated"
	environment.Set["MCP_SUPERVISOR_HELPER"] = "mutated"
	if process == nil || !report.Started || report.Policy.Enforcement != sandbox.EnforcementDisabled {
		t.Fatalf("start = process:%v report:%+v", process, report)
	}

	var output struct {
		Dir     string   `json:"dir"`
		Args    []string `json:"args"`
		Secret  string   `json:"secret"`
		Control string   `json:"control"`
	}
	if err := json.NewDecoder(process.Stdout()).Decode(&output); err != nil {
		t.Fatalf("decode helper output: %v", err)
	}
	if output.Args[0] != "semi;colon" || output.Args[1] != "space value" {
		t.Fatalf("argv = %#v", output.Args)
	}
	canonical, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTestPath(output.Dir, canonical) {
		t.Fatalf("dir = %q, want canonical %q", output.Dir, canonical)
	}
	if output.Secret != "" || output.Control != "inspect" {
		t.Fatalf("environment leaked or mutated: %+v", output)
	}
	if err := process.Wait(context.Background()); err != nil {
		t.Fatalf("wait helper: %v", err)
	}
}

func TestHostRunnerRejectsIsolatingPolicyWithoutStarting(t *testing.T) {
	process, report := NewHostRunner().Start(context.Background(), sandbox.Policy{
		Enforcement: sandbox.EnforcementPreferred,
	}, Spec{Executable: os.Args[0], Dir: t.TempDir()})
	if process != nil || report.Started || report.Failure != sandbox.FailureCapabilityUnavailable {
		t.Fatalf("process=%v report=%+v", process, report)
	}
}

func TestUnavailableRunnerFailsClosed(t *testing.T) {
	process, report := NewUnavailableRunner("adapter absent").Start(context.Background(), sandbox.Policy{
		Enforcement: sandbox.EnforcementRequired,
	}, Spec{Executable: os.Args[0], Dir: t.TempDir()})
	if process != nil || report.Started || report.Failure != sandbox.FailureRunnerUnavailable {
		t.Fatalf("process=%v report=%+v", process, report)
	}
}

func TestHostRunnerCancellationReapsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	process, report := NewHostRunner().Start(ctx, disabledTestPolicy(), Spec{
		Executable: os.Args[0],
		Args:       []string{"-test.run=^TestProcessSupervisorHelper$"},
		Dir:        t.TempDir(),
		Environment: sandbox.EnvironmentSpec{Set: map[string]string{
			"MCP_SUPERVISOR_HELPER": "block",
		}},
	})
	if process == nil || !report.Started {
		t.Fatalf("start report: %+v", report)
	}
	cancel()
	select {
	case <-process.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("canceled process was not reaped")
	}
}

func TestHostRunnerCancellationKillsUnixDescendantGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		if NewHostRunner().Capabilities().ProcessTreeKill {
			t.Fatal("Windows host adapter must not claim process-tree kill")
		}
		t.Skip("the Disabled Windows adapter truthfully guarantees only direct-child termination")
	}
	ctx, cancel := context.WithCancel(context.Background())
	process, report := NewHostRunner().Start(ctx, disabledTestPolicy(), Spec{
		Executable: os.Args[0], Args: []string{"-test.run=^TestProcessSupervisorHelper$"}, Dir: t.TempDir(),
		Environment: sandbox.EnvironmentSpec{Set: map[string]string{"MCP_SUPERVISOR_HELPER": "tree-parent"}},
	})
	if process == nil || !report.Capabilities.ProcessTreeKill {
		t.Fatalf("start report: %+v", report)
	}
	var child struct {
		PID int `json:"pid"`
	}
	if err := json.NewDecoder(process.Stdout()).Decode(&child); err != nil {
		t.Fatalf("decode child PID: %v", err)
	}
	cancel()
	select {
	case <-process.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("parent was not reaped")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		childProcess, _ := os.FindProcess(child.PID)
		if childProcess == nil || childProcess.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant %d survived process-group cancellation", child.PID)
}

func TestProcessSupervisorHelper(t *testing.T) {
	switch os.Getenv("MCP_SUPERVISOR_HELPER") {
	case "inspect":
		workingDir, _ := os.Getwd()
		separator := 0
		for i, argument := range os.Args {
			if argument == "--" {
				separator = i + 1
				break
			}
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"dir": workingDir, "args": os.Args[separator:],
			"secret":  os.Getenv("MCP_SUPERVISOR_AMBIENT_SECRET"),
			"control": os.Getenv("MCP_SUPERVISOR_HELPER"),
		})
		os.Exit(0)
	case "block":
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		time.Sleep(time.Hour)
	case "tree-parent":
		child := exec.Command(os.Args[0], "-test.run=^TestProcessSupervisorHelper$")
		child.Env = append(os.Environ(), "MCP_SUPERVISOR_HELPER=tree-child")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]int{"pid": child.Process.Pid})
		time.Sleep(time.Hour)
	case "tree-child":
		time.Sleep(time.Hour)
	default:
		return
	}
}

func disabledTestPolicy() sandbox.Policy {
	return sandbox.Policy{
		Enforcement: sandbox.EnforcementDisabled,
		Required: sandbox.Capabilities{
			EnvironmentFiltering: true,
			Timeouts:             true,
		},
	}
}

func sameTestPath(left, right string) bool {
	relative, err := filepath.Rel(strings.TrimSpace(left), strings.TrimSpace(right))
	return err == nil && relative == "."
}
