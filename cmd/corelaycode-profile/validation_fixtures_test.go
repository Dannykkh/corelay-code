package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
)

// Run only the authored fixture bytes, never output supplied by a provider.
// Production acceptance separately executes matching sources through the
// required-isolation runner in probe_verification.go.
func TestValidationGoFixturesFailBeforeRepairAndPassAfterRepair(t *testing.T) {
	plan := capabilityprofile.DefaultProbePlan()
	seen := map[capabilityprofile.ProbeCategory]bool{}
	for _, probeCase := range plan.Cases() {
		switch probeCase.Category {
		case capabilityprofile.CategoryMultiFileBug, capabilityprofile.CategoryNewFeatureTest, capabilityprofile.CategoryFixFailingTest:
		default:
			continue
		}
		if seen[probeCase.Category] {
			continue
		}
		seen[probeCase.Category] = true
		t.Run(string(probeCase.Category), func(t *testing.T) {
			workspace := t.TempDir()
			fixture, err := prepareAgentProbeFixture(capabilityprofile.ProbeExecution{Case: probeCase, Attempt: 1, WorkspaceRoot: workspace})
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"test", "-count=1"}
			for _, artifact := range fixture.artifactFiles {
				args = append(args, artifact.path)
				if strings.HasSuffix(artifact.path, "_test.go") {
					if err := os.WriteFile(artifact.path, artifact.expected, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			run := func() ([]byte, error) {
				cmd := exec.Command("go", args...)
				cmd.Dir = workspace
				return cmd.CombinedOutput()
			}
			if output, err := run(); err == nil || !strings.Contains(string(output), "--- FAIL: Test") {
				t.Fatalf("initial fixture must fail an executed assertion: err=%v output=%s", err, output)
			}
			for _, artifact := range fixture.artifactFiles {
				if err := os.WriteFile(artifact.path, artifact.expected, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if output, err := run(); err != nil {
				t.Fatalf("repaired fixture must pass: %v\n%s", err, output)
			}
		})
	}
}

func TestValidationFixtureCatalogHasExecutableAcceptanceArtifacts(t *testing.T) {
	plan := capabilityprofile.DefaultProbePlan()
	want := map[capabilityprofile.ProbeCategory]int{
		capabilityprofile.CategoryMultiFileBug:      2,
		capabilityprofile.CategoryNewFeatureTest:    2,
		capabilityprofile.CategoryFixFailingTest:    2,
		capabilityprofile.CategoryInterruptRecovery: 1,
	}
	seen := make(map[capabilityprofile.ProbeCategory]bool, len(want))
	for _, probeCase := range plan.Cases() {
		artifactCount, ok := want[probeCase.Category]
		if !ok {
			continue
		}
		seen[probeCase.Category] = true
		workspace := t.TempDir()
		fixture, err := prepareAgentProbeFixture(capabilityprofile.ProbeExecution{
			PlanVersion: plan.Version(), PlanDigest: plan.Digest(), Variant: plan.Variant(),
			Case: probeCase, Attempt: 1, WorkspaceRoot: workspace,
		})
		if err != nil {
			t.Fatalf("category %q fixture: %v", probeCase.Category, err)
		}
		if len(fixture.artifactFiles) != artifactCount || len(fixture.approvedMutations) == 0 && probeCase.Category != capabilityprofile.CategoryDecisionRetention {
			t.Fatalf("category %q artifacts=%d approvals=%d, want artifacts=%d", probeCase.Category, len(fixture.artifactFiles), len(fixture.approvedMutations), artifactCount)
		}
		if !strings.Contains(fixture.prompt, fixture.marker) {
			t.Fatalf("category %q prompt omitted marker", probeCase.Category)
		}
		for _, artifact := range fixture.artifactFiles {
			if !pathWithinFixture(workspace, artifact.path) {
				t.Fatalf("category %q escaped workspace: %q", probeCase.Category, artifact.path)
			}
			if err := os.WriteFile(artifact.path, artifact.expected, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if digest, ok := validateProbeArtifact(fixture); !ok || digest == "" {
			t.Fatalf("category %q acceptance did not pass: digest=%q", probeCase.Category, digest)
		}
	}
	for category := range want {
		if !seen[category] {
			t.Fatalf("default plan omitted validation category %q", category)
		}
	}
}

func pathWithinFixture(workspace, target string) bool {
	workspace, _ = filepath.EvalSymlinks(workspace)
	target, _ = filepath.EvalSymlinks(target)
	if workspace == "" || target == "" {
		return false
	}
	relative, err := filepath.Rel(workspace, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
