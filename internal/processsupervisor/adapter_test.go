package processsupervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func TestAutoRunnerRoutesDisabledOnlyToHost(t *testing.T) {
	secure := &recordingStreamingRunner{name: "secure", capabilities: sandbox.Capabilities{FilesystemIsolation: true}}
	host := &recordingStreamingRunner{name: "host"}
	runner := NewAutoRunnerWithOptions(AutoRunnerOptions{
		Platform: "fixture",
		Factories: map[string]AdapterFactory{
			"fixture": func(AdapterDependencies) Runner { return secure },
		},
		Unconfined: host,
	})
	for _, enforcement := range []sandbox.Enforcement{sandbox.EnforcementRequired, sandbox.EnforcementPreferred, sandbox.EnforcementDisabled} {
		_, _ = runner.Start(context.Background(), sandbox.Policy{Enforcement: enforcement}, Spec{})
	}
	if secure.calls != 2 || host.calls != 1 {
		t.Fatalf("secure calls=%d host calls=%d", secure.calls, host.calls)
	}
	if host.lastPolicy.Enforcement != sandbox.EnforcementDisabled {
		t.Fatalf("host policy=%#v", host.lastPolicy)
	}
}

func TestAutoRunnerNeverFallsBackWhenSecureAdapterUnavailable(t *testing.T) {
	host := &recordingStreamingRunner{name: "host"}
	runner := NewAutoRunnerWithOptions(AutoRunnerOptions{
		Platform: "fixture",
		Factories: map[string]AdapterFactory{
			"fixture": func(AdapterDependencies) Runner { return NewUnavailableRunner("fixture unavailable") },
		},
		Unconfined: host,
	})
	for _, enforcement := range []sandbox.Enforcement{sandbox.EnforcementRequired, sandbox.EnforcementPreferred} {
		process, report := runner.Start(context.Background(), sandbox.Policy{Enforcement: enforcement}, Spec{})
		if process != nil || report.Started || report.Failure != sandbox.FailureRunnerUnavailable {
			t.Fatalf("%s process=%v report=%#v", enforcement, process, report)
		}
	}
	if host.calls != 0 {
		t.Fatalf("secure failure fell back to host %d time(s)", host.calls)
	}
}

func TestBubblewrapFactoryProbesCapabilitiesAndRejectsMissingNetworkNamespace(t *testing.T) {
	var probes [][]string
	runner := newBubblewrapAdapter(AdapterDependencies{
		Lookup: func(name string) (string, error) {
			if name != "bwrap" {
				t.Fatalf("lookup=%q", name)
			}
			return "/fixture/bwrap", nil
		},
		Probe: func(_ string, args []string) error {
			probes = append(probes, append([]string(nil), args...))
			if containsArgument(args, "--unshare-net") {
				return errors.New("network namespaces unavailable")
			}
			return nil
		},
		Host: &recordingStreamingRunner{name: "host", capabilities: sandbox.Capabilities{ProcessTreeKill: true}},
	})
	capabilities := runner.Capabilities()
	if len(probes) != 2 || !capabilities.FilesystemIsolation || !capabilities.ProcessIsolation || capabilities.NetworkIsolation {
		t.Fatalf("probes=%#v capabilities=%#v", probes, capabilities)
	}
	process, report := runner.Start(context.Background(), sandbox.Policy{
		Enforcement: sandbox.EnforcementRequired, Workspace: t.TempDir(), WorkspaceAccess: sandbox.WorkspaceReadOnly,
		Network: sandbox.NetworkDenied,
	}, Spec{Executable: "/bin/true", Dir: t.TempDir()})
	if process != nil || report.Started || report.Failure != sandbox.FailureCapabilityUnavailable {
		t.Fatalf("process=%v report=%#v", process, report)
	}
}

func TestBuildBubblewrapSpecPreservesArgvAndWorkspaceRules(t *testing.T) {
	workspace := t.TempDir()
	subdirectory := filepath.Join(workspace, "sub")
	if err := os.Mkdir(subdirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	original := sandbox.EnvironmentSpec{Set: map[string]string{"VISIBLE": "yes"}}
	prepared, err := buildBubblewrapSpec("/fixture/bwrap", sandbox.Capabilities{NetworkIsolation: true}, sandbox.Policy{
		Enforcement: sandbox.EnforcementRequired, Workspace: workspace, WorkspaceAccess: sandbox.WorkspaceReadWrite,
		Network: sandbox.NetworkDenied,
	}, Spec{Executable: "/usr/bin/tool", Args: []string{"semi;colon", "space value"}, Dir: "sub", Environment: original})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	canonical, _ := canonicalWorkspace(workspace)
	for _, sequence := range [][]string{
		{"--ro-bind", "/", "/"}, {"--tmpfs", "/tmp"}, {"--unshare-net"},
		{"--bind", canonical, canonical}, {"--chdir", filepath.Join(canonical, "sub")},
		{"--", "/usr/bin/tool", "semi;colon", "space value"},
	} {
		if !containsArgumentSequence(prepared.Spec.Args, sequence) {
			t.Fatalf("args missing %#v: %#v", sequence, prepared.Spec.Args)
		}
	}
	if prepared.Spec.Environment.Set["TMPDIR"] != "/tmp" || prepared.Spec.Environment.Set["VISIBLE"] != "yes" {
		t.Fatalf("environment=%#v", prepared.Spec.Environment)
	}
	if _, mutated := original.Set["TMPDIR"]; mutated {
		t.Fatal("builder mutated caller environment")
	}
}

func TestBuildBubblewrapSpecRejectsSymlinkWorkingDirectoryEscape(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows symlink privileges are environment-dependent")
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(workspace, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := buildBubblewrapSpec("/fixture/bwrap", sandbox.Capabilities{}, sandbox.Policy{
		Enforcement: sandbox.EnforcementRequired, Workspace: workspace, WorkspaceAccess: sandbox.WorkspaceReadOnly,
	}, Spec{Executable: "/bin/true", Dir: link})
	var typed *adapterError
	if !errors.As(err, &typed) || typed.code != sandbox.FailureCommandInvalid {
		t.Fatalf("error=%T %v", err, err)
	}
}

func TestBuildSeatbeltSpecUsesDefaultDenyAndCleansTemporaryDirectory(t *testing.T) {
	workspace := t.TempDir()
	prepared, err := buildSeatbeltSpec("/usr/bin/sandbox-exec", func(pattern string) (string, error) {
		return os.MkdirTemp("", pattern)
	}, sandbox.Policy{
		Enforcement: sandbox.EnforcementRequired, Workspace: workspace, WorkspaceAccess: sandbox.WorkspaceReadWrite,
		Network: sandbox.NetworkDenied,
	}, Spec{Executable: "/usr/bin/tool", Args: []string{"arg"}})
	if err != nil {
		t.Fatal(err)
	}
	profile := prepared.Spec.Args[1]
	canonical, _ := canonicalWorkspace(workspace)
	temporary := prepared.Spec.Environment.Set["TMPDIR"]
	if !strings.Contains(profile, "(deny default)") ||
		!strings.Contains(profile, "file-write* (subpath "+strconv.Quote(canonical)) ||
		!strings.Contains(profile, strconv.Quote(temporary)) || strings.Contains(profile, "(allow network*)") {
		t.Fatalf("profile=%s", profile)
	}
	prepared.Cleanup()
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatalf("temporary directory remains: %v", err)
	}
}

type recordingStreamingRunner struct {
	name         string
	capabilities sandbox.Capabilities
	calls        int
	lastPolicy   sandbox.Policy
}

func (r *recordingStreamingRunner) Name() string { return r.name }
func (r *recordingStreamingRunner) Capabilities() sandbox.Capabilities {
	return r.capabilities
}
func (r *recordingStreamingRunner) Start(_ context.Context, policy sandbox.Policy, _ Spec) (*Process, Report) {
	r.calls++
	r.lastPolicy = policy
	return nil, Report{Runner: r.name, Policy: policy, Capabilities: r.capabilities}
}

func containsArgument(args []string, wanted string) bool {
	for _, argument := range args {
		if argument == wanted {
			return true
		}
	}
	return false
}

func containsArgumentSequence(args, sequence []string) bool {
	for start := 0; start+len(sequence) <= len(args); start++ {
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
	return len(sequence) == 0
}
