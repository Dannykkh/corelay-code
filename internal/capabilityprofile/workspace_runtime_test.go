package capabilityprofile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type runtimeTestProvisioner struct {
	proof      RuntimeIsolationProof
	prepareErr error
	guard      *runtimeTestGuard
	roots      []string
}

func (p *runtimeTestProvisioner) Prepare(_ context.Context, root string, request WorkspaceRequest) (IsolationGuard, error) {
	p.roots = append(p.roots, root)
	if p.prepareErr != nil {
		return nil, p.prepareErr
	}
	proof := p.proof
	if proof.OutboundTargetDigest == "" {
		proof.OutboundTargetDigest = request.TargetDigest
	}
	guard := &runtimeTestGuard{proof: proof}
	p.guard = guard
	return guard, nil
}

type runtimeTestGuard struct {
	proof  RuntimeIsolationProof
	closed int
	err    error
}

func (g *runtimeTestGuard) Proof() RuntimeIsolationProof { return g.proof }
func (g *runtimeTestGuard) Close() error {
	g.closed++
	return g.err
}

func validRuntimeTestProof() RuntimeIsolationProof {
	return RuntimeIsolationProof{
		SchemaVersion: CurrentRuntimeIsolationProofSchemaVersion,
		Ready:         true, DisposableWorkspace: true, AgentExecutionIsolated: true,
		FilesystemBoundaryEnforced: true, OutboundAllowlistEnforced: true,
		EvidenceDigest: digestText("external-isolation-evidence"),
		Mechanism:      "test-isolator-v1",
	}
}

func TestDisposableWorkspaceFactoryUsesUniqueRootsAndDeletesAfterGuardRelease(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "workspaces")
	provisioner := &runtimeTestProvisioner{proof: validRuntimeTestProof()}
	factory, err := NewDisposableWorkspaceFactory(parent, provisioner)
	if err != nil {
		t.Fatal(err)
	}
	target := testTarget(t)
	request := WorkspaceRequest{
		TargetDigest: target.Digest(), PlanDigest: digestText("plan"),
		CaseID: "case-1", Attempt: 1,
	}
	first, err := factory.Acquire(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	firstRoot := first.Root()
	if _, err := os.Stat(firstRoot); err != nil || !validIsolationProof(first.Proof(), target) {
		t.Fatalf("workspace=%q proof=%+v stat=%v", firstRoot, first.Proof(), err)
	}
	canary, err := os.ReadFile(filepath.Join(filepath.Dir(firstRoot), RuntimeBoundaryCanaryName))
	if err != nil || string(canary) != string(RuntimeBoundaryCanary(request)) {
		t.Fatalf("boundary canary=%q error=%v", canary, err)
	}
	if !strings.HasPrefix(string(canary), "corelay-profile-boundary-v1:") {
		t.Fatalf("boundary canary uses legacy identity: %q", canary)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if provisioner.guard.closed != 1 {
		t.Fatalf("guard close count = %d", provisioner.guard.closed)
	}
	if _, err := os.Stat(firstRoot); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists after close: %v", err)
	}

	second, err := factory.Acquire(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.Root() == firstRoot || second.Proof().WorkspaceDigest == first.Proof().WorkspaceDigest {
		t.Fatal("disposable attempts reused workspace identity")
	}
}

func TestDisposableWorkspaceFactoryRejectsUnprovenNetworkBeforeLease(t *testing.T) {
	proof := validRuntimeTestProof()
	proof.OutboundAllowlistEnforced = false
	provisioner := &runtimeTestProvisioner{proof: proof}
	factory, err := NewDisposableWorkspaceFactory(filepath.Join(t.TempDir(), "workspaces"), provisioner)
	if err != nil {
		t.Fatal(err)
	}
	_, err = factory.Acquire(context.Background(), WorkspaceRequest{
		TargetDigest: digestText("target"), PlanDigest: digestText("plan"),
		CaseID: "case-1", Attempt: 1,
	})
	if !errors.Is(err, ErrIsolationUnavailable) {
		t.Fatalf("Acquire error = %v, want ErrIsolationUnavailable", err)
	}
	if provisioner.guard == nil || provisioner.guard.closed != 1 {
		t.Fatal("invalid proof did not release its guard")
	}
	entries, readErr := os.ReadDir(factory.root)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("disposable residue = %v, error=%v", entries, readErr)
	}
}

func TestDisposableWorkspaceFactoryRequiresProvisioner(t *testing.T) {
	if _, err := NewDisposableWorkspaceFactory(t.TempDir(), nil); !errors.Is(err, ErrIsolationUnavailable) {
		t.Fatalf("NewDisposableWorkspaceFactory error = %v", err)
	}
}

func TestInvalidRuntimeProofQuarantinesWithoutCallingExecutor(t *testing.T) {
	proof := validRuntimeTestProof()
	proof.FilesystemBoundaryEnforced = false
	factory, err := NewDisposableWorkspaceFactory(
		filepath.Join(t.TempDir(), "workspaces"),
		&runtimeTestProvisioner{proof: proof},
	)
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeExecutor{}
	profiler, err := NewProfiler(factory, executor, ProfilerConfig{
		Clock: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := profiler.Run(context.Background(), testTarget(t), testProbePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 0 || profile.Verified() || !hasReason(profile, QuarantineIsolation) {
		t.Fatalf("calls=%d verified=%v reasons=%v", executor.calls, profile.Verified(), profile.QuarantineReasons())
	}
}
