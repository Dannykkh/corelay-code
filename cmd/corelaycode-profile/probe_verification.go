package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func isCodingProbe(category capabilityprofile.ProbeCategory) bool {
	switch category {
	case capabilityprofile.CategoryMultiFileBug, capabilityprofile.CategoryNewFeatureTest, capabilityprofile.CategoryFixFailingTest:
		return true
	}
	return false
}

func verifyCodingProbeObservation(ctx context.Context, fixture agentProbeFixture, category capabilityprofile.ProbeCategory, observation *observedAgentProbe, runner sandbox.Runner) {
	if !isCodingProbe(category) || !observation.value.Success {
		return
	}
	started := time.Now()
	code, report := runCodingProbeVerification(ctx, fixture, runner)
	observation.value.Latency += time.Since(started)
	observation.value.TraceDigest = digestJSON(struct {
		Previous     string
		Verification string
		Sandbox      sandbox.Report
	}{observation.value.TraceDigest, code, report})
	if code != "" {
		observation.value.Success = false
		observation.value.FalseDone = true
		observation.transportFailed = true
		observation.failureCode = code
	}
}

// Only snapshot-matching sources are copied into this independent workspace.
// The provider's process-denying runner stays unchanged; executable acceptance
// has a separate mandatory isolation boundary and no host fallback.
func runCodingProbeVerification(ctx context.Context, fixture agentProbeFixture, runner sandbox.Runner) (string, sandbox.Report) {
	empty := sandbox.Report{}
	if ctx.Err() != nil {
		return "verification_canceled", empty
	}
	if _, matches := validateProbeArtifact(fixture); !matches || len(fixture.artifactFiles) == 0 {
		return "verification_snapshot_mismatch", empty
	}
	executable, err := exec.LookPath("go")
	if err != nil {
		return "verification_go_unavailable", empty
	}
	workspace, err := os.MkdirTemp("", "corelay-probe-verification-")
	if err != nil {
		return "verification_workspace_unavailable", empty
	}
	defer os.RemoveAll(workspace)
	args := []string{"test", "-json", "-count=1", "-timeout=30s"}
	for _, artifact := range fixture.artifactFiles {
		name := filepath.Base(artifact.path)
		if filepath.Ext(name) != ".go" {
			return "verification_invalid_artifact", empty
		}
		// Copy the checked acceptance bytes rather than rereading a mutable path.
		if err := os.WriteFile(filepath.Join(workspace, name), artifact.expected, 0o600); err != nil {
			return "verification_workspace_unavailable", empty
		}
		args = append(args, name)
	}
	policy := sandbox.Policy{Enforcement: sandbox.EnforcementRequired, Workspace: workspace, WorkspaceAccess: sandbox.WorkspaceReadWrite, Network: sandbox.NetworkDenied,
		Required: sandbox.Capabilities{FilesystemIsolation: true, NetworkIsolation: true, ProcessIsolation: true, ProcessTreeKill: true, EnvironmentFiltering: true, Timeouts: true}}
	command := sandbox.CommandSpec{Path: executable, Args: args, Dir: workspace, Timeout: 90 * time.Second, OutputLimitBytes: 1 << 20,
		Environment: sandbox.EnvironmentSpec{Inherit: []string{"PATH", "SystemRoot", "WINDIR"}, Set: map[string]string{"GOCACHE": filepath.Join(workspace, "cache"), "GOPATH": filepath.Join(workspace, "gopath"), "GOENV": "off", "GOWORK": "off", "GO111MODULE": "off", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "CGO_ENABLED": "0"}}}
	result, report := runner.Run(ctx, policy, command)
	// Detail is backend-controlled prose and is not part of profiler evidence.
	report.Detail = ""
	if ctx.Err() != nil || result.Canceled {
		return "verification_canceled", report
	}
	if result.TimedOut {
		return "verification_timeout", report
	}
	if !result.Started || !report.Started || report.EffectiveEnforcement != sandbox.EnforcementRequired || !report.AppliedIsolation.FilesystemIsolation || !report.AppliedIsolation.NetworkIsolation || !report.AppliedIsolation.ProcessIsolation {
		return "verification_isolation_unavailable", report
	}
	if result.Err != nil || result.ExitCode != 0 || result.OutputTruncated || report.Failure != sandbox.FailureNone {
		return "verification_tests_failed", report
	}
	scanner := bufio.NewScanner(bytes.NewReader(result.Stdout))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	tests := 0
	for scanner.Scan() {
		var event struct{ Action, Test string }
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return "verification_invalid_test_output", report
		}
		if event.Action == "fail" {
			return "verification_tests_failed", report
		}
		if event.Action == "pass" && event.Test != "" {
			tests++
		}
	}
	if scanner.Err() != nil || tests == 0 {
		return "verification_no_tests", report
	}
	return "", report
}
