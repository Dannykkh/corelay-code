package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type verificationRunner struct {
	run func(context.Context, sandbox.Policy, sandbox.CommandSpec) (sandbox.Result, sandbox.Report)
}

func (r verificationRunner) Name() string                       { return "verification-fixture" }
func (r verificationRunner) Capabilities() sandbox.Capabilities { return sandbox.Capabilities{} }
func (r verificationRunner) Run(ctx context.Context, p sandbox.Policy, c sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
	return r.run(ctx, p, c)
}

func verificationFixture(t *testing.T, passing bool) agentProbeFixture {
	t.Helper()
	root := t.TempDir()
	body := "package fixture\nimport \"testing\"\nfunc TestAcceptance(t *testing.T) {"
	if !passing {
		body += "t.Fatal(\"broken\")"
	}
	body += "}\n"
	path := filepath.Join(root, "acceptance_test.go")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return agentProbeFixture{artifactFiles: []probeArtifact{{path: path, expected: []byte(body)}}}
}

func TestCodingProbeVerificationRequiresExecutedTestsAndIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
		exit               int
		isolated           bool
	}{
		{"pass", "{\"Action\":\"pass\",\"Test\":\"TestAcceptance\"}\n", "", 0, true},
		{"failed assertion", "{\"Action\":\"fail\",\"Test\":\"TestAcceptance\"}\n", "verification_tests_failed", 1, true},
		{"no tests", "{\"Action\":\"pass\"}\n", "verification_no_tests", 0, true},
		{"host execution forbidden", "{\"Action\":\"pass\",\"Test\":\"TestAcceptance\"}\n", "verification_isolation_unavailable", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := verificationFixture(t, true)
			var checkedWorkspace string
			runner := verificationRunner{run: func(ctx context.Context, p sandbox.Policy, c sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
				checkedWorkspace = p.Workspace
				if p.Enforcement != sandbox.EnforcementRequired || p.Network != sandbox.NetworkDenied || !p.Required.ProcessIsolation || p.Workspace == filepath.Dir(fixture.artifactFiles[0].path) {
					t.Fatal("verification boundary missing")
				}
				if c.Environment.Set["GOPROXY"] != "off" || c.Environment.Set["GOENV"] != "off" || c.OutputLimitBytes <= 0 || c.Timeout <= 0 || !strings.Contains(strings.Join(c.Args, " "), "-json") {
					t.Fatal("unbounded/non-deterministic command")
				}
				isolation := sandbox.Capabilities{FilesystemIsolation: tc.isolated, NetworkIsolation: tc.isolated, ProcessIsolation: tc.isolated}
				return sandbox.Result{Started: true, Stdout: []byte(tc.output), ExitCode: tc.exit}, sandbox.Report{Started: true, EffectiveEnforcement: sandbox.EnforcementRequired, AppliedIsolation: isolation}
			}}
			observation := observedAgentProbe{value: capabilityprofile.ProbeObservation{Success: true, TraceDigest: "previous"}}
			verifyCodingProbeObservation(context.Background(), fixture, capabilityprofile.CategoryNewFeatureTest, &observation, runner)
			if observation.failureCode != tc.want || observation.value.Success != (tc.want == "") {
				t.Fatalf("observation=%+v", observation)
			}
			if _, err := os.Stat(checkedWorkspace); !os.IsNotExist(err) {
				t.Fatal("verification workspace not cleaned")
			}
		})
	}
}

func TestCodingProbeVerificationCancellationAndUnavailable(t *testing.T) {
	fixture := verificationFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, _ := runCodingProbeVerification(ctx, fixture, verificationRunner{run: func(context.Context, sandbox.Policy, sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		t.Fatal("canceled verifier started runner")
		return sandbox.Result{}, sandbox.Report{}
	}})
	if code != "verification_canceled" {
		t.Fatal(code)
	}
	code, _ = runCodingProbeVerification(context.Background(), fixture, sandbox.NewUnavailableRunner("unavailable"))
	if code != "verification_isolation_unavailable" {
		t.Fatal(code)
	}
	ctx, cancel = context.WithCancel(context.Background())
	code, _ = runCodingProbeVerification(ctx, fixture, verificationRunner{run: func(ctx context.Context, _ sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
		cancel()
		<-ctx.Done()
		return sandbox.Result{Canceled: true}, sandbox.Report{}
	}})
	if code != "verification_canceled" {
		t.Fatal(code)
	}
}

func TestCodingProbeVerificationRealLinuxIsolation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux bubblewrap integration")
	}
	runner := sandbox.NewAutoRunner()
	for _, passing := range []bool{false, true} {
		code, report := runCodingProbeVerification(context.Background(), verificationFixture(t, passing), runner)
		if code == "verification_isolation_unavailable" {
			t.Skipf("required isolation unavailable: %+v", report)
		}
		want := "verification_tests_failed"
		if passing {
			want = ""
		}
		if code != want {
			t.Fatalf("passing=%v code=%s report=%+v", passing, code, report)
		}
	}
}
