package agent

import (
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func TestLintFile_Go(t *testing.T) {
	dir := t.TempDir()
	opts := secureGoLintFixtureOptions(t)

	valid := filepath.Join(dir, "ok.go")
	validContent := []byte("package x\n\nfunc F() int { return 1 }\n")
	os.WriteFile(valid, validContent, 0644)
	if got := lintFileWithOptions(valid, artifactBytesRevision(validContent), opts); !got.Valid {
		t.Errorf("valid go was flagged: %+v", got)
	}

	broken := filepath.Join(dir, "bad.go")
	brokenContent := []byte("package x\n\nfunc broken( {\n")
	os.WriteFile(broken, brokenContent, 0644)
	if got := lintFileWithOptions(broken, artifactBytesRevision(brokenContent), opts); got.Valid || got.Failure != LintFailureSyntax {
		t.Error("broken go syntax was NOT detected")
	}
}

func secureGoLintFixtureOptions(t *testing.T) ToolExecutionOptions {
	t.Helper()
	runner := &fakeToolProcessRunner{
		name:         "fake-go-lint",
		capabilities: fakeBashCapabilities(),
	}
	runner.run = func(_ context.Context, policy sandbox.Policy, command sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		report := sandbox.Report{
			Runner:               runner.Name(),
			RequestedEnforcement: policy.Enforcement,
			EffectiveEnforcement: policy.Enforcement,
			Capabilities:         runner.Capabilities(),
			Started:              true,
		}
		if command.Path != "gofmt" || len(command.Args) == 0 {
			t.Fatalf("unexpected Go lint command: %#v", command)
		}
		_, err := parser.ParseFile(token.NewFileSet(), command.Args[len(command.Args)-1], nil, parser.AllErrors)
		if err != nil {
			return sandbox.Result{
				Started:  true,
				Stderr:   []byte(err.Error()),
				ExitCode: 2,
				Err:      err,
			}, report
		}
		return sandbox.Result{Started: true, ExitCode: 0}, report
	}
	return secureToolProcessOptions(runner)
}

func TestLintFile_JSON(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "ok.json")
	os.WriteFile(good, []byte(`{"a":1}`), 0644)
	if got := lintFile(good); got != "" {
		t.Errorf("valid json was flagged: %s", got)
	}

	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{ not valid"), 0644)
	if got := lintFile(bad); got == "" {
		t.Error("invalid json was NOT detected")
	}
}

func TestLintFile_UnknownExtPasses(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.xyz")
	os.WriteFile(f, []byte("garbage {{{ not a real language"), 0644)
	if got := lintFile(f); got != "" {
		t.Errorf("unknown extension should never block, got: %s", got)
	}
}
