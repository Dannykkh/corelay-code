package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAutoRunnerRoutesDisabledOnlyToUnconfined(t *testing.T) {
	secure := &recordingRunner{name: "secure", capabilities: Capabilities{FilesystemIsolation: true}}
	unconfined := &recordingRunner{name: "unconfined", capabilities: Capabilities{EnvironmentFiltering: true, Timeouts: true}}
	auto := NewAutoRunnerWithOptions(AutoRunnerOptions{
		Platform: "fixture",
		Factories: map[string]AdapterFactory{
			"fixture": func(AdapterDependencies) Runner { return secure },
		},
		Unconfined: unconfined,
	})

	auto.Run(context.Background(), Policy{Enforcement: EnforcementRequired}, CommandSpec{})
	auto.Run(context.Background(), Policy{Enforcement: EnforcementPreferred}, CommandSpec{})
	auto.Run(context.Background(), Policy{Enforcement: EnforcementDisabled}, CommandSpec{})
	if secure.calls != 2 || unconfined.calls != 1 {
		t.Fatalf("secure calls=%d unconfined calls=%d", secure.calls, unconfined.calls)
	}
	if unconfined.lastPolicy.Enforcement != EnforcementDisabled {
		t.Fatalf("unconfined received policy %#v", unconfined.lastPolicy)
	}
}

func TestAutoRunnerNeverFallsBackWhenSecureAdapterUnavailable(t *testing.T) {
	unconfined := &recordingRunner{name: "unconfined", capabilities: Capabilities{EnvironmentFiltering: true, Timeouts: true}}
	auto := NewAutoRunnerWithOptions(AutoRunnerOptions{
		Platform: "fixture",
		Factories: map[string]AdapterFactory{
			"fixture": func(AdapterDependencies) Runner {
				return NewUnavailableRunner("secure fixture unavailable")
			},
		},
		Unconfined: unconfined,
	})
	for _, enforcement := range []Enforcement{EnforcementRequired, EnforcementPreferred} {
		result, report := auto.Run(context.Background(), Policy{Enforcement: enforcement}, CommandSpec{})
		if result.Started || report.Failure != FailureRunnerUnavailable {
			t.Fatalf("%s result=%#v report=%#v", enforcement, result, report)
		}
	}
	if unconfined.calls != 0 {
		t.Fatalf("unavailable secure adapter fell back %d times", unconfined.calls)
	}
}

func TestBubblewrapFactoryUsesLookupAndCapabilityProbes(t *testing.T) {
	var lookedUp string
	var probes [][]string
	dependencies := AdapterDependencies{
		Lookup: func(name string) (string, error) {
			lookedUp = name
			return "/fixture/bwrap", nil
		},
		Probe: func(_ string, args []string) error {
			probes = append(probes, append([]string(nil), args...))
			if containsArg(args, "--unshare-net") {
				return errors.New("network namespaces disabled")
			}
			return nil
		},
		Host: &recordingRunner{name: "host", capabilities: Capabilities{EnvironmentFiltering: true, Timeouts: true}},
	}
	runner := newBubblewrapAdapter(dependencies)
	if lookedUp != "bwrap" || len(probes) != 2 {
		t.Fatalf("lookup=%q probes=%#v", lookedUp, probes)
	}
	capabilities := runner.Capabilities()
	if !capabilities.FilesystemIsolation || !capabilities.ProcessIsolation || capabilities.NetworkIsolation {
		t.Fatalf("capabilities = %#v", capabilities)
	}
	result, report := runner.Run(context.Background(), Policy{
		Enforcement:     EnforcementRequired,
		Workspace:       t.TempDir(),
		WorkspaceAccess: WorkspaceReadWrite,
		Network:         NetworkDenied,
	}, CommandSpec{Path: "/bin/true"})
	if result.Started || report.Failure != FailureCapabilityUnavailable {
		t.Fatalf("network-deny result=%#v report=%#v", result, report)
	}
}

func TestBubblewrapFactoryFailsClosedWhenLookupOrProbeFails(t *testing.T) {
	tests := []struct {
		name   string
		lookup ExecutableLookup
		probe  CommandProbe
	}{
		{
			name: "missing binary",
			lookup: func(string) (string, error) {
				return "", errors.New("missing")
			},
			probe: func(string, []string) error { return nil },
		},
		{
			name:   "base capability unavailable",
			lookup: func(string) (string, error) { return "/fixture/bwrap", nil },
			probe:  func(string, []string) error { return errors.New("namespace denied") },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := newBubblewrapAdapter(AdapterDependencies{Lookup: test.lookup, Probe: test.probe})
			result, report := runner.Run(context.Background(), Policy{Enforcement: EnforcementRequired}, CommandSpec{})
			if result.Started || report.Failure != FailureRunnerUnavailable {
				t.Fatalf("result=%#v report=%#v", result, report)
			}
		})
	}
}

func TestBuildBubblewrapCommand(t *testing.T) {
	workspace := t.TempDir()
	subDir := filepath.Join(workspace, "sub")
	if err := os.Mkdir(subDir, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	originalEnvironment := EnvironmentSpec{Set: map[string]string{"VISIBLE": "yes"}}
	prepared, err := buildBubblewrapCommand(
		"/fixture/bwrap",
		Capabilities{NetworkIsolation: true},
		Policy{
			Enforcement:     EnforcementRequired,
			Workspace:       workspace,
			WorkspaceAccess: WorkspaceReadWrite,
			Network:         NetworkDenied,
		},
		CommandSpec{
			Path:             "/usr/bin/tool",
			Args:             []string{"one", "two"},
			Dir:              "sub",
			Environment:      originalEnvironment,
			OutputLimitBytes: 123,
		},
	)
	if err != nil {
		t.Fatalf("buildBubblewrapCommand: %v", err)
	}
	canonical, err := canonicalWorkspace(workspace)
	if err != nil {
		t.Fatalf("canonicalWorkspace: %v", err)
	}
	wantSequences := [][]string{
		{"--ro-bind", "/", "/"},
		{"--tmpfs", "/tmp"},
		{"--unshare-net"},
		{"--bind", canonical, canonical},
		{"--chdir", filepath.Join(canonical, "sub")},
		{"--", "/usr/bin/tool", "one", "two"},
	}
	for _, sequence := range wantSequences {
		if !containsArgSequence(prepared.Command.Args, sequence) {
			t.Fatalf("args missing %q: %#v", sequence, prepared.Command.Args)
		}
	}
	if prepared.Command.Environment.Set["TMPDIR"] != "/tmp" || prepared.Command.Environment.Set["VISIBLE"] != "yes" {
		t.Fatalf("environment = %#v", prepared.Command.Environment)
	}
	if _, mutated := originalEnvironment.Set["TMPDIR"]; mutated {
		t.Fatal("builder mutated caller environment")
	}
	if prepared.Command.OutputLimitBytes != 123 {
		t.Fatalf("output limit = %d, want 123", prepared.Command.OutputLimitBytes)
	}
	if !prepared.AppliedIsolation.FilesystemIsolation || !prepared.AppliedIsolation.NetworkIsolation || !prepared.AppliedIsolation.ProcessIsolation {
		t.Fatalf("applied isolation = %#v", prepared.AppliedIsolation)
	}
}

func TestBuildBubblewrapCommandRejectsWorkingDirOutsideWorkspace(t *testing.T) {
	_, err := buildBubblewrapCommand(
		"/fixture/bwrap",
		Capabilities{},
		Policy{Enforcement: EnforcementRequired, Workspace: t.TempDir(), WorkspaceAccess: WorkspaceReadOnly},
		CommandSpec{Path: "/bin/true", Dir: filepath.Dir(t.TempDir())},
	)
	var runnerErr *RunnerError
	if !errors.As(err, &runnerErr) || runnerErr.Code != FailureCommandInvalid {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestSeatbeltFactoryUsesLookupAndProbe(t *testing.T) {
	var probeArgs []string
	runner := newSeatbeltAdapter(AdapterDependencies{
		Lookup: func(name string) (string, error) {
			if name != "sandbox-exec" {
				t.Fatalf("lookup name = %q", name)
			}
			return "/usr/bin/sandbox-exec", nil
		},
		Probe: func(_ string, args []string) error {
			probeArgs = append([]string(nil), args...)
			return nil
		},
		TempDir: func(pattern string) (string, error) { return os.MkdirTemp("", pattern) },
		Host:    &recordingRunner{name: "host", capabilities: Capabilities{EnvironmentFiltering: true, Timeouts: true}},
	})
	if !runner.Capabilities().FilesystemIsolation || !runner.Capabilities().NetworkIsolation {
		t.Fatalf("capabilities = %#v", runner.Capabilities())
	}
	if len(probeArgs) < 3 || probeArgs[0] != "-p" || !strings.Contains(probeArgs[1], "(deny default)") {
		t.Fatalf("probe args = %#v", probeArgs)
	}
}

func TestBuildSeatbeltCommandDefaultDenyRulesAndCleanup(t *testing.T) {
	workspace := t.TempDir()
	prepared, err := buildSeatbeltCommand(
		"/usr/bin/sandbox-exec",
		func(pattern string) (string, error) { return os.MkdirTemp("", pattern) },
		Policy{
			Enforcement:     EnforcementRequired,
			Workspace:       workspace,
			WorkspaceAccess: WorkspaceReadWrite,
			Network:         NetworkDenied,
		},
		CommandSpec{Path: "/usr/bin/true", OutputLimitBytes: 456},
	)
	if err != nil {
		t.Fatalf("buildSeatbeltCommand: %v", err)
	}
	if prepared.Cleanup == nil {
		t.Fatal("missing temp cleanup")
	}
	profile := prepared.Command.Args[1]
	canonical, _ := canonicalWorkspace(workspace)
	if !strings.Contains(profile, "(deny default)") ||
		!strings.Contains(profile, "file-write* (subpath "+strconv.Quote(canonical)) ||
		!strings.Contains(profile, strconv.Quote(prepared.Command.Environment.Set["TMPDIR"])) {
		t.Fatalf("profile = %s", profile)
	}
	if strings.Contains(profile, "(allow network*)") {
		t.Fatalf("network deny profile allows network: %s", profile)
	}
	if prepared.Command.OutputLimitBytes != 456 {
		t.Fatalf("output limit = %d, want 456", prepared.Command.OutputLimitBytes)
	}
	tempDir := prepared.Command.Environment.Set["TMPDIR"]
	if _, err := os.Stat(tempDir); err != nil {
		t.Fatalf("isolated temp missing before cleanup: %v", err)
	}
	if err := prepared.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Fatalf("isolated temp still exists: %v", err)
	}
}

func TestSeatbeltNetworkAllowIsExplicit(t *testing.T) {
	profile := buildSeatbeltProfile("/workspace", "/private/tmp/isolated", Policy{
		WorkspaceAccess: WorkspaceReadOnly,
		Network:         NetworkAllowed,
	})
	if !strings.Contains(profile, "(deny default)") || !strings.Contains(profile, "(allow network*)") {
		t.Fatalf("profile = %s", profile)
	}
	if strings.Contains(profile, "file-write* (subpath \"/workspace\")") {
		t.Fatalf("read-only workspace became writable: %s", profile)
	}
}

func TestPolicyRulesImplyCapabilities(t *testing.T) {
	policy := Policy{
		Enforcement:      EnforcementRequired,
		Workspace:        t.TempDir(),
		WorkspaceAccess:  WorkspaceReadOnly,
		Network:          NetworkDenied,
		MaxProcesses:     2,
		MemoryLimitBytes: 1024,
	}
	required := policy.RequiredCapabilities()
	if !required.FilesystemIsolation || !required.NetworkIsolation || !required.ProcessLimits || !required.MemoryLimits {
		t.Fatalf("required capabilities = %#v", required)
	}
	err := ValidatePolicy(policy, Capabilities{FilesystemIsolation: true, ProcessIsolation: true})
	var policyErr *PolicyError
	if !errors.As(err, &policyErr) || policyErr.Code != FailureCapabilityUnavailable {
		t.Fatalf("error = %T %v", err, err)
	}
}

type recordingRunner struct {
	name         string
	capabilities Capabilities
	calls        int
	lastPolicy   Policy
}

func (r *recordingRunner) Name() string { return r.name }

func (r *recordingRunner) Capabilities() Capabilities { return r.capabilities }

func (r *recordingRunner) Run(_ context.Context, policy Policy, _ CommandSpec) (Result, Report) {
	r.calls++
	r.lastPolicy = policy
	return Result{Started: true, ExitCode: 0}, Report{
		Runner:               r.name,
		RequestedEnforcement: policy.Enforcement,
		EffectiveEnforcement: policy.Enforcement,
		Capabilities:         r.capabilities,
		Started:              true,
	}
}

func containsArg(args []string, wanted string) bool {
	for _, argument := range args {
		if argument == wanted {
			return true
		}
	}
	return false
}

func containsArgSequence(args, sequence []string) bool {
	if len(sequence) == 0 || len(sequence) > len(args) {
		return false
	}
	for start := 0; start <= len(args)-len(sequence); start++ {
		matches := true
		for offset := range sequence {
			if args[start+offset] != sequence[offset] {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}
