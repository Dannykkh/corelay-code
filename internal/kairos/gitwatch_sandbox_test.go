package kairos

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type gitWatchFakeRunner struct {
	commands []sandbox.CommandSpec
	policy   []sandbox.Policy
	fail     bool
}

func (*gitWatchFakeRunner) Name() string { return "git-watch-fake" }
func (*gitWatchFakeRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{ProcessIsolation: true, ProcessTreeKill: true, EnvironmentFiltering: true, Timeouts: true}
}
func (r *gitWatchFakeRunner) Run(_ context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
	r.commands = append(r.commands, command)
	r.policy = append(r.policy, policy)
	if r.fail {
		return sandbox.Result{Started: false, ExitCode: sandbox.ExitNotStarted, Err: errors.New("raw-secret-must-not-leak")}, sandbox.Report{Runner: r.Name(), Failure: sandbox.FailureStartFailed, Detail: "raw-secret-must-not-leak"}
	}
	output := ""
	switch strings.Join(command.Args, " ") {
	case "branch --show-current":
		output = "main\n"
	case "status --porcelain=v1 --untracked-files=normal":
		output = " M changed.go\n?\n?? new.go\n"
	case "rev-list --count @{u}..HEAD":
		output = "2\n"
	case "log -1 --format=%s|||%ci":
		output = "subject|||2026-08-12 00:00:00 +0900\n"
	}
	return sandbox.Result{Started: true, ExitCode: 0, Stdout: []byte(output)}, sandbox.Report{Runner: r.Name(), Started: true}
}

func TestCheckGitStatusUsesSupervisedFixedArgv(t *testing.T) {
	runner := &gitWatchFakeRunner{}
	workDir := t.TempDir()
	reports := 0
	status, err := CheckGitStatusWithOptions(workDir, GitExecutionOptions{
		Context: context.Background(), Runner: runner,
		Policy:        sandbox.Policy{Enforcement: sandbox.EnforcementPreferred},
		Timeout:       time.Second,
		ObserveReport: func(sandbox.Report) { reports++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if status.Branch != "main" || status.AheadBy != 2 || !reflect.DeepEqual(status.Modified, []string{"changed.go"}) || !reflect.DeepEqual(status.Untracked, []string{"new.go"}) {
		t.Fatalf("status = %#v", status)
	}
	if reports != 5 || len(runner.commands) != 5 {
		t.Fatalf("reports=%d commands=%d", reports, len(runner.commands))
	}
	canonical, _ := filepath.EvalSymlinks(workDir)
	for _, command := range runner.commands {
		if command.Path != "git" || command.Dir != canonical || command.Timeout != time.Second {
			t.Fatalf("command = %#v", command)
		}
		if !reflect.DeepEqual(command.Environment.Set, map[string]string(nil)) || len(command.Environment.Inherit) == 0 {
			t.Fatalf("environment = %#v", command.Environment)
		}
	}
}

func TestCheckGitStatusFailureDoesNotExposeRunnerError(t *testing.T) {
	runner := &gitWatchFakeRunner{fail: true}
	_, err := CheckGitStatusWithOptions(t.TempDir(), GitExecutionOptions{
		Context: context.Background(), Runner: runner,
		Policy:  sandbox.Policy{Enforcement: sandbox.EnforcementPreferred},
		Timeout: time.Second,
	})
	if err == nil || strings.Contains(err.Error(), "raw-secret") {
		t.Fatalf("error = %v", err)
	}
}

func TestCheckGitStatusRejectsMissingExecutionBoundary(t *testing.T) {
	_, err := CheckGitStatusWithOptions(t.TempDir(), GitExecutionOptions{})
	if err == nil {
		t.Fatal("zero execution options were accepted")
	}
}
