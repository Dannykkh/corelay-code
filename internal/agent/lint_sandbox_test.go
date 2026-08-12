package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func TestLintFileWithOptionsUsesRunnerArgvContextAndReport(t *testing.T) {
	type contextKey string
	const key contextKey = "lint-test"
	path := filepath.Join(t.TempDir(), "main.go")
	content := []byte("package main\n\nfunc main() {}\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeToolProcessRunner{name: "fake-lint", capabilities: fakeBashCapabilities()}
	runner.run = func(ctx context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		if ctx.Value(key) != "preserved" {
			t.Fatalf("caller context value was not propagated")
		}
		return sandbox.Result{Started: true, ExitCode: 0}, sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
		}
	}
	var observed sandbox.Report
	opts := secureToolProcessOptions(runner)
	opts.Context = context.WithValue(context.Background(), key, "preserved")
	opts.ObserveSandbox = func(report sandbox.Report) { observed = report }

	result := lintFileWithOptions(path, artifactBytesRevision(content), opts)
	if !result.Checked || !result.Valid || result.Failure != LintFailureNone {
		t.Fatalf("lint result=%#v", result)
	}
	commands := runner.commandSnapshot()
	if len(commands) != 1 || commands[0].Path != "gofmt" || !reflect.DeepEqual(commands[0].Args, []string{"-e", path}) {
		t.Fatalf("commands=%#v", commands)
	}
	if observed.Runner != runner.Name() || !observed.Started || result.SandboxReport.Runner != runner.Name() {
		t.Fatalf("observed=%#v result report=%#v", observed, result.SandboxReport)
	}
}

func TestWriteLintSandboxFailureRollsBackNewArtifactWithoutFallback(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "new.go")
	input, _ := json.Marshal(map[string]string{
		"file_path": "new.go",
		"content":   "package main\n\nfunc main() {}\n",
	})
	runner := &fakeToolProcessRunner{name: "fake-lint", capabilities: fakeBashCapabilities()}
	opts := secureToolProcessOptions(runner)
	opts.SandboxPolicy = sandbox.Policy{Enforcement: sandbox.EnforcementDisabled}
	var observed sandbox.Report
	opts.ObserveSandbox = func(report sandbox.Report) { observed = report }

	output, isError := executeWriteV2WithOptions(input, workDir, opts)
	if !isError || !strings.Contains(output, "rolled back") {
		t.Fatalf("output=%q isError=%v", output, isError)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("new artifact survived failed lint: %v", err)
	}
	if len(runner.commandSnapshot()) != 0 {
		t.Fatalf("disabled policy reached runner: %#v", runner.commandSnapshot())
	}
	if observed.Failure != sandbox.FailurePolicyInvalid || observed.Started {
		t.Fatalf("observed report=%#v", observed)
	}
}

func TestExecuteToolWithOptionsPlumbsWriteLintSandbox(t *testing.T) {
	workDir := t.TempDir()
	runner := &fakeToolProcessRunner{name: "fake-lint", capabilities: fakeBashCapabilities()}
	input, _ := json.Marshal(map[string]string{
		"file_path": "main.go",
		"content":   "package main\n\nfunc main() {}\n",
	})
	output, isError := ExecuteToolWithOptions("Write", input, workDir, secureToolProcessOptions(runner))
	if isError || !strings.Contains(output, "Created") {
		t.Fatalf("output=%q isError=%v", output, isError)
	}
	commands := runner.commandSnapshot()
	if len(commands) != 1 || commands[0].Path != "gofmt" {
		t.Fatalf("commands=%#v", commands)
	}
}

func TestEditLintSyntaxFailureRestoresOwnedRevision(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "main.go")
	original := "package main\n\nfunc main() {\n\tprintln(\"ok\")\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeToolProcessRunner{name: "fake-lint", capabilities: fakeBashCapabilities()}
	call := 0
	runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		call++
		report := sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
		}
		if call == 1 {
			return sandbox.Result{Started: true, ExitCode: 0}, report
		}
		report.Failure = sandbox.FailureExecutionFailed
		return sandbox.Result{
			Started:  true,
			Stderr:   []byte("main.go: missing }"),
			ExitCode: 2,
			Err:      errors.New("exit status 2"),
		}, report
	}
	input := editInput(t, "main.go", "\tprintln(\"ok\")\n}", "\tprintln(\"ok\")")
	output, isError := executeEditV2WithOptions(input, workDir, secureToolProcessOptions(runner))
	if !isError || !strings.Contains(output, "syntax error") {
		t.Fatalf("output=%q isError=%v", output, isError)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != original {
		t.Fatalf("rollback content=%q err=%v", after, err)
	}
	if call != 2 {
		t.Fatalf("lint calls=%d, want baseline and candidate", call)
	}
}

func TestWriteLintCancellationRollsBack(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "cancel.go")
	input, _ := json.Marshal(map[string]string{
		"file_path": "cancel.go",
		"content":   "package cancel\n",
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := &fakeToolProcessRunner{name: "fake-lint", capabilities: fakeBashCapabilities()}
	opts := secureToolProcessOptions(runner)
	opts.Context = ctx

	output, isError := executeWriteV2WithOptions(input, workDir, opts)
	if !isError || !strings.Contains(output, "rolled back") {
		t.Fatalf("output=%q isError=%v", output, isError)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("canceled write survived: %v", err)
	}
	if len(runner.commandSnapshot()) != 0 {
		t.Fatalf("pre-canceled context reached runner")
	}
}

func TestWriteLintTimeoutRollsBack(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "timeout.go")
	input, _ := json.Marshal(map[string]string{
		"file_path": "timeout.go",
		"content":   "package timeout\n",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	runner := &fakeToolProcessRunner{name: "fake-lint", capabilities: fakeBashCapabilities()}
	runner.run = func(ctx context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		<-ctx.Done()
		return sandbox.Result{Started: true, ExitCode: -1, TimedOut: true, Err: ctx.Err()}, sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
			Failure:              sandbox.FailureTimedOut,
		}
	}
	opts := secureToolProcessOptions(runner)
	opts.Context = ctx

	output, isError := executeWriteV2WithOptions(input, workDir, opts)
	if !isError || !strings.Contains(output, "rolled back") {
		t.Fatalf("output=%q isError=%v", output, isError)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("timed-out write survived: %v", err)
	}
}

func TestEditLintConcurrentExternalChangeIsNotOverwritten(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "main.go")
	original := "package main\n\nfunc main() {\n\tprintln(\"before\")\n}\n"
	external := "package main\n\nfunc main() {\n\tprintln(\"external\")\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeToolProcessRunner{name: "fake-lint", capabilities: fakeBashCapabilities()}
	call := 0
	runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		call++
		report := sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
		}
		if call == 1 {
			return sandbox.Result{Started: true, ExitCode: 0}, report
		}
		if err := os.WriteFile(path, []byte(external), 0o600); err != nil {
			t.Fatal(err)
		}
		report.Failure = sandbox.FailureExecutionFailed
		return sandbox.Result{Started: true, ExitCode: 2, Err: errors.New("exit status 2")}, report
	}
	input := editInput(t, "main.go", "before", "candidate")
	output, isError := executeEditV2WithOptions(input, workDir, secureToolProcessOptions(runner))
	if !isError || !strings.Contains(output, "changed concurrently") || !strings.Contains(output, "Rollback was skipped") {
		t.Fatalf("output=%q isError=%v", output, isError)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != external {
		t.Fatalf("external edit was overwritten: content=%q err=%v", after, err)
	}
}

func TestRollbackArtifactRevisionMismatchDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "main.go")
	owned := []byte("package owned\n")
	external := []byte("package external\n")
	if err := os.WriteFile(path, owned, 0o600); err != nil {
		t.Fatal(err)
	}
	expected := artifactBytesRevision(owned)
	if err := os.WriteFile(path, external, 0o600); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := rollbackArtifactIfRevision(path, expected, true, []byte("package original\n"))
	if err == nil || rolledBack || !strings.Contains(err.Error(), "changed concurrently") {
		t.Fatalf("rolledBack=%v err=%v", rolledBack, err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !reflect.DeepEqual(after, external) {
		t.Fatalf("external content changed: %q err=%v", after, readErr)
	}
}

func TestExistingEditFailsBeforeMutationWhenSandboxUnavailable(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, "main.go")
	original := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	output, isError := executeEditV2WithOptions(
		editInput(t, "main.go", "main", "changed"),
		workDir,
		ToolExecutionOptions{},
	)
	if !isError || !strings.Contains(output, "not started") {
		t.Fatalf("output=%q isError=%v", output, isError)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != original {
		t.Fatalf("unavailable lint mutated file: content=%q err=%v", after, err)
	}
}

func TestLintUsesWindowsSandboxAdapterOnCurrentHost(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows adapter contract")
	}
	workDir := t.TempDir()
	path := filepath.Join(workDir, "main.go")
	content := []byte("package main\n\nfunc main() {}\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	runner, policy := DefaultSandboxExecution(workDir)
	result := lintFileWithOptions(path, artifactBytesRevision(content), ToolExecutionOptions{
		Context:       context.Background(),
		SandboxRunner: runner,
		SandboxPolicy: policy,
	})
	if !result.Valid || result.Failure != LintFailureNone {
		t.Fatalf("result=%#v", result)
	}
	if result.SandboxReport.Runner != "windows-job-object" || !result.SandboxReport.AppliedIsolation.ProcessIsolation {
		t.Fatalf("report=%#v", result.SandboxReport)
	}
}
