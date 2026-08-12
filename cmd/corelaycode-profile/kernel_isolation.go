package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/providers"
)

const profileKernelIsolationMechanism = "corelaycode-kernel-target-v1"

// kernelIsolationProvisioner attests the model-controlled execution boundary
// assembled by agentProbeExecutor: a disposable workspace, strict workspace
// path/approval checks, no hooks/plugins/MCP, no model-issued processes,
// scoped HOME/config, offline optional tools, and one target-bound provider
// HTTP client. It is an application/kernel boundary, not an OS sandbox for the
// whole corelaycode-profile process.
type kernelIsolationProvisioner struct {
	target    capabilityprofile.TargetIdentity
	transport providers.TargetBindingProof
}

func newKernelIsolationProvisioner(
	target capabilityprofile.TargetIdentity,
	transport providers.TargetBindingProof,
) (capabilityprofile.IsolationProvisioner, error) {
	if !target.Valid() || !validTargetBindingProof(transport) ||
		transport.EndpointDigest != target.EndpointDigest() {
		return nil, capabilityprofile.ErrIsolationUnavailable
	}
	return &kernelIsolationProvisioner{target: target, transport: transport}, nil
}

func (p *kernelIsolationProvisioner) Prepare(
	ctx context.Context,
	workspace string,
	request capabilityprofile.WorkspaceRequest,
) (capabilityprofile.IsolationGuard, error) {
	if p == nil || !p.target.Valid() || !validTargetBindingProof(p.transport) ||
		p.transport.EndpointDigest != p.target.EndpointDigest() ||
		request.TargetDigest != p.target.Digest() {
		return nil, capabilityprofile.ErrIsolationUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	if workspace == "" || !filepath.IsAbs(workspace) {
		return nil, capabilityprofile.ErrIsolationUnavailable
	}
	workspaceDigest := sha256.Sum256([]byte(workspace))
	evidence := digestKernelIsolationEvidence(
		request,
		hex.EncodeToString(workspaceDigest[:]),
		p.transport,
	)
	return &kernelIsolationGuard{proof: capabilityprofile.RuntimeIsolationProof{
		SchemaVersion:              capabilityprofile.CurrentRuntimeIsolationProofSchemaVersion,
		Ready:                      true,
		DisposableWorkspace:        true,
		AgentExecutionIsolated:     true,
		FilesystemBoundaryEnforced: true,
		OutboundAllowlistEnforced:  true,
		OutboundTargetDigest:       request.TargetDigest,
		EvidenceDigest:             evidence,
		Mechanism:                  profileKernelIsolationMechanism,
	}}, nil
}

type kernelIsolationGuard struct {
	mu     sync.Mutex
	proof  capabilityprofile.RuntimeIsolationProof
	closed bool
}

func (g *kernelIsolationGuard) Proof() capabilityprofile.RuntimeIsolationProof {
	if g == nil {
		return capabilityprofile.RuntimeIsolationProof{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return capabilityprofile.RuntimeIsolationProof{}
	}
	return g.proof
}

func (g *kernelIsolationGuard) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	return nil
}

func validTargetBindingProof(proof providers.TargetBindingProof) bool {
	return proof.Version == providers.TargetBindingProofVersion &&
		proof.Scope == providers.TargetBindingScope &&
		validProfileDigest(proof.EndpointDigest) &&
		validProfileDigest(proof.TargetDigest) &&
		validProfileDigest(proof.HostHash) &&
		validProfileDigest(proof.IPSetHash) &&
		validProfileDigest(proof.PathHash) &&
		proof.AddressCount > 0 && proof.AddressCount <= 256
}

func validProfileDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func digestKernelIsolationEvidence(
	request capabilityprofile.WorkspaceRequest,
	workspaceDigest string,
	transport providers.TargetBindingProof,
) string {
	payload := struct {
		Mechanism       string `json:"mechanism"`
		TargetDigest    string `json:"targetDigest"`
		PlanDigest      string `json:"planDigest"`
		CaseID          string `json:"caseId"`
		Attempt         int    `json:"attempt"`
		WorkspaceDigest string `json:"workspaceDigest"`
		Transport       struct {
			Version        string `json:"version"`
			Scope          string `json:"scope"`
			EndpointDigest string `json:"endpointDigest"`
			TargetDigest   string `json:"targetDigest"`
			HostHash       string `json:"hostHash"`
			IPSetHash      string `json:"ipSetHash"`
			PathHash       string `json:"pathHash"`
			AddressCount   int    `json:"addressCount"`
		} `json:"transport"`
	}{
		Mechanism:    profileKernelIsolationMechanism,
		TargetDigest: request.TargetDigest, PlanDigest: request.PlanDigest,
		CaseID: request.CaseID, Attempt: request.Attempt,
		WorkspaceDigest: workspaceDigest,
	}
	payload.Transport.Version = transport.Version
	payload.Transport.Scope = transport.Scope
	payload.Transport.EndpointDigest = transport.EndpointDigest
	payload.Transport.TargetDigest = transport.TargetDigest
	payload.Transport.HostHash = transport.HostHash
	payload.Transport.IPSetHash = transport.IPSetHash
	payload.Transport.PathHash = transport.PathHash
	payload.Transport.AddressCount = transport.AddressCount
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// digestProfileFields is used only by tests and content-free proof fixtures.
// Length prefixes prevent ambiguous concatenations.
func digestProfileFields(fields ...string) string {
	hash := sha256.New()
	var size [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(field))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

var _ capabilityprofile.IsolationProvisioner = (*kernelIsolationProvisioner)(nil)
var _ capabilityprofile.IsolationGuard = (*kernelIsolationGuard)(nil)
