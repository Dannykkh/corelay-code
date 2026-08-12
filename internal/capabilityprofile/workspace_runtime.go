package capabilityprofile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const CurrentRuntimeIsolationProofSchemaVersion = 1

// RuntimeBoundaryCanaryName is created beside (not inside) every disposable
// workspace before the external isolation guard is applied. Safety probes can
// verify that agent-issued paths cannot mutate it without the trusted executor
// needing to create an out-of-workspace file after enforcement starts.
const RuntimeBoundaryCanaryName = ".corelay-profile-boundary-canary"

// RuntimeIsolationProof is an attestation from the component that actually
// owns the filesystem and network boundary. A digest alone is insufficient:
// all enforcement claims must be explicit and bound to the selected target.
// AgentExecutionIsolated means every model-controlled Executor path is inside
// the attested boundary: provider calls are target-bound, ambient home/config
// reads are scoped, filesystem tools are workspace-bound, and executable
// extension/process surfaces are either isolated or disabled. It does not by
// itself claim that unrelated goroutines in the host process run in an OS
// sandbox. Raw endpoints and credentials are intentionally absent.
type RuntimeIsolationProof struct {
	SchemaVersion              int    `json:"schemaVersion"`
	Ready                      bool   `json:"ready"`
	DisposableWorkspace        bool   `json:"disposableWorkspace"`
	AgentExecutionIsolated     bool   `json:"agentExecutionIsolated"`
	FilesystemBoundaryEnforced bool   `json:"filesystemBoundaryEnforced"`
	OutboundAllowlistEnforced  bool   `json:"outboundAllowlistEnforced"`
	OutboundTargetDigest       string `json:"outboundTargetDigest"`
	EvidenceDigest             string `json:"evidenceDigest"`
	Mechanism                  string `json:"mechanism"`
}

// IsolationGuard owns an already-applied boundary for one workspace. Close
// must release that boundary; the workspace is deleted only after Close.
type IsolationGuard interface {
	Proof() RuntimeIsolationProof
	Close() error
}

// IsolationProvisioner is the minimal production seam missing from the
// in-process agent kernel. Implementations are expected to be preconfigured
// with the raw endpoint they enforce, while this interface receives and
// returns only its one-way target digest.
type IsolationProvisioner interface {
	Prepare(context.Context, string, WorkspaceRequest) (IsolationGuard, error)
}

// DisposableWorkspaceFactory creates a fresh workspace for every attempt and
// delegates enforcement to an injected isolation owner. It never treats a
// temporary directory by itself as filesystem or network isolation.
type DisposableWorkspaceFactory struct {
	root        string
	provisioner IsolationProvisioner
}

func NewDisposableWorkspaceFactory(root string, provisioner IsolationProvisioner) (*DisposableWorkspaceFactory, error) {
	if provisioner == nil {
		return nil, ErrIsolationUnavailable
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("%w: disposable workspace parent is empty", ErrInvalidRuntime)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: disposable workspace parent is invalid", ErrInvalidRuntime)
	}
	absolute = filepath.Clean(absolute)
	if err := ensureSecureDirectory(absolute, true); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, ErrUnsafeStorePath
	}
	return &DisposableWorkspaceFactory{root: canonical, provisioner: provisioner}, nil
}

func (f *DisposableWorkspaceFactory) Acquire(ctx context.Context, request WorkspaceRequest) (WorkspaceLease, error) {
	if f == nil || f.provisioner == nil || strings.TrimSpace(f.root) == "" {
		return nil, ErrIsolationUnavailable
	}
	if err := validateWorkspaceRequest(request); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ensureSecureDirectory(f.root, false); err != nil {
		return nil, err
	}
	container, err := os.MkdirTemp(f.root, ".corelay-profile-")
	if err != nil {
		return nil, fmt.Errorf("create disposable capability workspace: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = removeDisposableContainer(f.root, container)
		}
	}()
	if err := os.Chmod(container, 0o700); err != nil {
		return nil, fmt.Errorf("secure disposable capability workspace: %w", err)
	}
	canonicalContainer, err := filepath.EvalSymlinks(container)
	if err != nil || !pathIsDirectChild(f.root, canonicalContainer) {
		return nil, ErrUnsafeStorePath
	}
	workspace := filepath.Join(canonicalContainer, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		return nil, fmt.Errorf("create capability workspace root: %w", err)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil || !pathIsDirectChild(canonicalContainer, canonicalWorkspace) {
		return nil, ErrUnsafeStorePath
	}
	canaryPath := filepath.Join(canonicalContainer, RuntimeBoundaryCanaryName)
	if err := os.WriteFile(canaryPath, RuntimeBoundaryCanary(request), 0o600); err != nil {
		return nil, fmt.Errorf("create capability boundary canary: %w", err)
	}

	guard, err := f.provisioner.Prepare(ctx, canonicalWorkspace, request)
	if err != nil {
		if guard != nil {
			_ = guard.Close()
		}
		return nil, ErrIsolationUnavailable
	}
	if guard == nil {
		return nil, ErrIsolationUnavailable
	}
	proof := guard.Proof()
	if !validRuntimeIsolationProof(proof, request.TargetDigest) {
		_ = guard.Close()
		return nil, ErrIsolationUnavailable
	}
	workspaceDigest := runtimeWorkspaceDigest(request, canonicalWorkspace, proof)
	lease := &disposableWorkspaceLease{
		parent: f.root, container: canonicalContainer, workspace: canonicalWorkspace,
		guard: guard,
		proof: IsolationProof{
			Ready:                true,
			WorkspaceDigest:      workspaceDigest,
			OutboundTargetDigest: request.TargetDigest,
		},
	}
	cleanup = false
	return lease, nil
}

type disposableWorkspaceLease struct {
	parent    string
	container string
	workspace string
	guard     IsolationGuard
	proof     IsolationProof
	closeOnce sync.Once
	closeErr  error
}

func (l *disposableWorkspaceLease) Root() string {
	if l == nil {
		return ""
	}
	return l.workspace
}

func (l *disposableWorkspaceLease) Proof() IsolationProof {
	if l == nil {
		return IsolationProof{}
	}
	return l.proof
}

func (l *disposableWorkspaceLease) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.guard != nil {
			l.closeErr = l.guard.Close()
		}
		if removeErr := removeDisposableContainer(l.parent, l.container); l.closeErr == nil {
			l.closeErr = removeErr
		}
	})
	return l.closeErr
}

func validateWorkspaceRequest(request WorkspaceRequest) error {
	if !validDigest(request.TargetDigest) || !validDigest(request.PlanDigest) ||
		!safeIdentifier.MatchString(strings.TrimSpace(request.CaseID)) ||
		request.Attempt <= 0 || request.Attempt > maxProbeRepeats {
		return ErrInvalidRuntime
	}
	return nil
}

func validRuntimeIsolationProof(proof RuntimeIsolationProof, targetDigest string) bool {
	return proof.SchemaVersion == CurrentRuntimeIsolationProofSchemaVersion &&
		proof.Ready && proof.DisposableWorkspace && proof.AgentExecutionIsolated &&
		proof.FilesystemBoundaryEnforced &&
		proof.OutboundAllowlistEnforced && proof.OutboundTargetDigest == targetDigest &&
		validDigest(proof.OutboundTargetDigest) && validDigest(proof.EvidenceDigest) &&
		safeIdentifier.MatchString(strings.TrimSpace(proof.Mechanism))
}

func runtimeWorkspaceDigest(request WorkspaceRequest, workspace string, proof RuntimeIsolationProof) string {
	pathSum := sha256.Sum256([]byte(filepath.Clean(workspace)))
	payload := struct {
		SchemaVersion int    `json:"schemaVersion"`
		TargetDigest  string `json:"targetDigest"`
		PlanDigest    string `json:"planDigest"`
		CaseID        string `json:"caseId"`
		Attempt       int    `json:"attempt"`
		WorkspacePath string `json:"workspacePathDigest"`
		Evidence      string `json:"evidenceDigest"`
		Mechanism     string `json:"mechanism"`
	}{
		SchemaVersion: CurrentRuntimeIsolationProofSchemaVersion,
		TargetDigest:  request.TargetDigest, PlanDigest: request.PlanDigest,
		CaseID: request.CaseID, Attempt: request.Attempt,
		WorkspacePath: hex.EncodeToString(pathSum[:]), Evidence: proof.EvidenceDigest,
		Mechanism: proof.Mechanism,
	}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// RuntimeBoundaryCanary returns deterministic, non-secret canary bytes for a
// single attempt. The bytes contain only a digest of already-digested target
// and plan identity plus fixed probe metadata.
func RuntimeBoundaryCanary(request WorkspaceRequest) []byte {
	payload := struct {
		SchemaVersion int    `json:"schemaVersion"`
		TargetDigest  string `json:"targetDigest"`
		PlanDigest    string `json:"planDigest"`
		CaseID        string `json:"caseId"`
		Attempt       int    `json:"attempt"`
	}{
		SchemaVersion: CurrentRuntimeIsolationProofSchemaVersion,
		TargetDigest:  request.TargetDigest, PlanDigest: request.PlanDigest,
		CaseID: request.CaseID, Attempt: request.Attempt,
	}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return []byte("corelay-profile-boundary-v1:" + hex.EncodeToString(sum[:]) + "\n")
}

func pathIsDirectChild(parent, child string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	if err != nil || relative == "." || filepath.IsAbs(relative) {
		return false
	}
	return filepath.Dir(relative) == "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func removeDisposableContainer(parent, container string) error {
	parent = filepath.Clean(parent)
	container = filepath.Clean(container)
	base := filepath.Base(container)
	if !pathIsDirectChild(parent, container) ||
		(!strings.HasPrefix(base, ".corelay-profile-") &&
			!strings.HasPrefix(base, ".aniclew-profile-")) {
		return ErrUnsafeStorePath
	}
	info, err := os.Lstat(container)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafeStorePath
	}
	resolved, err := filepath.EvalSymlinks(container)
	if err != nil || !samePath(resolved, container) || !pathIsDirectChild(parent, resolved) {
		return ErrUnsafeStorePath
	}
	if err := os.RemoveAll(container); err != nil {
		return fmt.Errorf("remove disposable capability workspace: %w", err)
	}
	return nil
}
