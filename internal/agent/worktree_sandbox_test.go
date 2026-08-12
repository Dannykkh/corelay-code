package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func TestCreateWorktreeUsesRunnerArgvContextAndManagedScope(t *testing.T) {
	type contextKey string
	const key contextKey = "worktree"
	repo := t.TempDir()
	runner := &fakeToolProcessRunner{name: "fake-worktree", capabilities: fakeBashCapabilities()}
	runner.run = func(ctx context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		if ctx.Value(key) != "preserved" {
			t.Fatalf("caller context was not propagated")
		}
		report := sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
		}
		if len(command.Args) > 0 && command.Args[0] == "show-ref" {
			report.Failure = sandbox.FailureExecutionFailed
			return sandbox.Result{Started: true, ExitCode: 1, Err: errors.New("exit status 1")}, report
		}
		return sandbox.Result{Started: true, ExitCode: 0}, report
	}
	var reports []sandbox.Report
	opts := secureToolProcessOptions(runner)
	opts.Context = context.WithValue(context.Background(), key, "preserved")
	opts.ObserveSandbox = func(report sandbox.Report) { reports = append(reports, report) }

	worktree, err := CreateWorktreeWithOptions(repo, "Feature One", opts)
	if err != nil {
		t.Fatal(err)
	}
	wantBase := filepath.Join(worktree.ParentDir, managedWorktreeStateDir, managedWorktreeDir)
	if !pathWithin(worktree.Path, wantBase) || worktree.Branch != "corelaycode/feature-one" {
		t.Fatalf("worktree=%#v", worktree)
	}
	commands := runner.commandSnapshot()
	if len(commands) != 3 {
		t.Fatalf("commands=%#v", commands)
	}
	if !reflect.DeepEqual(commands[0].Args, []string{"rev-parse", "--git-dir"}) ||
		!reflect.DeepEqual(commands[1].Args, []string{"show-ref", "--verify", "--quiet", "refs/heads/corelaycode/feature-one"}) ||
		!reflect.DeepEqual(commands[2].Args, []string{"worktree", "add", "-b", "corelaycode/feature-one", worktree.Path}) {
		t.Fatalf("commands=%#v", commands)
	}
	if len(reports) != 3 || reports[2].Runner != runner.Name() {
		t.Fatalf("reports=%#v", reports)
	}
}

func TestWorktreeZeroDisabledAndCanceledFailBeforeRunner(t *testing.T) {
	repo := t.TempDir()
	runner := &fakeToolProcessRunner{name: "fake-worktree", capabilities: fakeBashCapabilities()}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name string
		opts ToolExecutionOptions
	}{
		{name: "zero", opts: ToolExecutionOptions{}},
		{name: "disabled", opts: ToolExecutionOptions{SandboxRunner: runner, SandboxPolicy: sandbox.Policy{Enforcement: sandbox.EnforcementDisabled}}},
		{name: "canceled", opts: ToolExecutionOptions{Context: canceled, SandboxRunner: runner, SandboxPolicy: fakeBashPolicy(sandbox.EnforcementRequired)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := len(runner.commandSnapshot())
			_, err := CreateWorktreeWithOptions(repo, "blocked", test.opts)
			if err == nil {
				t.Fatal("expected fail-closed error")
			}
			var processErr *WorktreeProcessError
			if !errors.As(err, &processErr) || processErr.Report.Started {
				t.Fatalf("error=%T %v", err, err)
			}
			if after := len(runner.commandSnapshot()); after != before {
				t.Fatalf("runner calls changed from %d to %d", before, after)
			}
		})
	}
}

func TestRemoveWorktreeRejectsOutsideManagedScopeWithoutRunnerOrHostFallback(t *testing.T) {
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-worktree")
	runner := &fakeToolProcessRunner{name: "fake-worktree", capabilities: fakeBashCapabilities()}
	err := RemoveWorktreeWithOptions(repo, outside, secureToolProcessOptions(runner))
	if err == nil || !strings.Contains(err.Error(), "outside managed scope") {
		t.Fatalf("error=%v", err)
	}
	if len(runner.commandSnapshot()) != 0 {
		t.Fatalf("outside path reached runner: %#v", runner.commandSnapshot())
	}
}

func TestListAndStatusWorktreesUseSameRunner(t *testing.T) {
	repo := t.TempDir()
	managed := filepath.Join(repo, managedWorktreeStateDir, managedWorktreeDir, "worker-one")
	if err := osMkdirAllForTest(managed); err != nil {
		t.Fatal(err)
	}
	runner := &fakeToolProcessRunner{name: "fake-worktree", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		output := ""
		if reflect.DeepEqual(command.Args, []string{"worktree", "list", "--porcelain"}) {
			output = "worktree " + managed + "\nbranch refs/heads/corelaycode/worker-one\n\n"
		} else if reflect.DeepEqual(command.Args, []string{"status", "--porcelain"}) {
			output = " M file.go\n"
		}
		return sandbox.Result{Started: true, Stdout: []byte(output), ExitCode: 0}, sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
		}
	}
	opts := secureToolProcessOptions(runner)
	worktrees, err := ListWorktreesWithOptions(repo, opts)
	if err != nil || len(worktrees) != 1 {
		t.Fatalf("worktrees=%#v err=%v", worktrees, err)
	}
	changed, err := worktrees[0].HasChangesWithOptions(opts)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
}

func TestTeamWorktreeCompositionUsesConfiguredRunner(t *testing.T) {
	repo := t.TempDir()
	runner := &fakeToolProcessRunner{name: "fake-worktree", capabilities: fakeBashCapabilities()}
	runner.run = func(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		return sandbox.Result{Started: true, ExitCode: 0}, sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
		}
	}
	team := NewTeam(nil, "", repo, t.TempDir(), TeamConfig{
		Name:          "sandbox-team",
		SandboxRunner: runner,
		SandboxPolicy: fakeBashPolicy(sandbox.EnforcementRequired),
	})
	if _, err := team.ListWorktrees(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands := runner.commandSnapshot()
	if len(commands) != 1 || commands[0].Path != "git" {
		t.Fatalf("commands=%#v", commands)
	}
}

func TestWorktreeUsesWindowsSandboxAdapterOnCurrentHost(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows adapter contract")
	}
	repo := t.TempDir()
	runner, policy := DefaultSandboxExecution(repo)
	opts := ToolExecutionOptions{
		Context:       context.Background(),
		SandboxRunner: runner,
		SandboxPolicy: policy,
	}
	init := runWorktreeGit(opts, repo, "initialize test repository", "init")
	if worktreeProcessFailed(init) {
		t.Fatalf("git init result=%#v", init)
	}
	var observed sandbox.Report
	opts.ObserveSandbox = func(report sandbox.Report) { observed = report }
	if _, err := ListWorktreesWithOptions(repo, opts); err != nil {
		t.Fatal(err)
	}
	if observed.Runner != "windows-job-object" || !observed.AppliedIsolation.ProcessIsolation {
		t.Fatalf("report=%#v", observed)
	}
}

func osMkdirAllForTest(path string) error {
	return os.MkdirAll(path, 0o755)
}
