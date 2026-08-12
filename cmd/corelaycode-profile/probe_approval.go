package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

var errProbeApprovalDenied = errors.New("capability probe approval denied")

type probeApprovedMutation struct {
	tool  string
	path  string
	input json.RawMessage
}

type probeApprovalGrant struct {
	tool      string
	path      string
	canonical string
	digest    string
}

// probeApprovalRequester grants only a precomputed fixture-local mutation.
// The exact tool input digest is consumed at Open, so cancellation or a failed
// Await can never turn one approval into a reusable capability.
type probeApprovalRequester struct {
	mu        sync.Mutex
	workspace string
	sessionID string
	allowed   map[string]probeApprovalGrant
	pending   map[string]approval.Pending
	clock     func() time.Time
}

func newProbeApprovalRequester(workspace, sessionID string, mutations []probeApprovedMutation) (*probeApprovalRequester, error) {
	canonicalWorkspace, err := canonicalProbeWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, capabilityprofile.ErrInvalidRuntime
	}
	requester := &probeApprovalRequester{
		workspace: canonicalWorkspace,
		sessionID: sessionID,
		allowed:   make(map[string]probeApprovalGrant, len(mutations)),
		pending:   make(map[string]approval.Pending, len(mutations)),
		clock:     time.Now,
	}
	for _, mutation := range mutations {
		if mutation.tool != "Edit" && mutation.tool != "Write" || len(mutation.input) == 0 || !json.Valid(mutation.input) {
			return nil, capabilityprofile.ErrInvalidRuntime
		}
		canonical, err := canonicalFixtureMutationPath(canonicalWorkspace, mutation.path)
		if err != nil {
			return nil, err
		}
		digest := probeToolInputDigest(mutation.tool, mutation.input)
		key := mutation.tool + "\x00" + digest
		if _, duplicate := requester.allowed[key]; duplicate {
			return nil, capabilityprofile.ErrInvalidRuntime
		}
		requester.allowed[key] = probeApprovalGrant{
			tool: mutation.tool, path: mutation.path, canonical: canonical, digest: digest,
		}
	}
	return requester, nil
}

func (r *probeApprovalRequester) Open(draft approval.Draft) (approval.Pending, error) {
	if r == nil || draft.SessionID != r.sessionID || strings.TrimSpace(draft.RunID) == "" ||
		strings.TrimSpace(draft.ToolCallID) == "" || draft.RememberAllowed {
		return approval.Pending{}, errProbeApprovalDenied
	}
	key := draft.ToolName + "\x00" + draft.InputDigest
	r.mu.Lock()
	defer r.mu.Unlock()
	grant, ok := r.allowed[key]
	if !ok || grant.digest != draft.InputDigest || grant.tool != draft.ToolName {
		return approval.Pending{}, errProbeApprovalDenied
	}
	path, err := approvalDraftPath(draft.RedactedInput)
	if err != nil || path != grant.path || draft.Scope != grant.path {
		return approval.Pending{}, errProbeApprovalDenied
	}
	canonical, err := canonicalFixtureMutationPath(r.workspace, path)
	if err != nil || !samePathText(canonical, grant.canonical) {
		return approval.Pending{}, errProbeApprovalDenied
	}
	delete(r.allowed, key)
	now := r.clock().UTC()
	idSum := sha256.Sum256([]byte(draft.SessionID + "\x00" + draft.RunID + "\x00" + draft.ToolCallID + "\x00" + draft.InputDigest))
	pending := approval.Pending{
		ID:        "profile_" + hex.EncodeToString(idSum[:16]),
		SessionID: draft.SessionID, SessionRevision: draft.SessionRevision,
		RunID: draft.RunID, ToolCallID: draft.ToolCallID, ToolName: draft.ToolName,
		RedactedInput: draft.RedactedInput, InputDigest: draft.InputDigest,
		DangerLevel: draft.DangerLevel, Scope: draft.Scope, RememberAllowed: false,
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	r.pending[pending.ID] = pending
	return pending, nil
}

func (r *probeApprovalRequester) Await(ctx context.Context, sessionID, approvalID string) (approval.Resolution, error) {
	if r == nil {
		return approval.Resolution{}, errProbeApprovalDenied
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return approval.Resolution{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.pending[approvalID]
	if !ok || pending.SessionID != sessionID || !pending.ExpiresAt.After(r.clock()) {
		return approval.Resolution{}, errProbeApprovalDenied
	}
	delete(r.pending, approvalID)
	return approval.Resolution{
		ApprovalID: pending.ID,
		Outcome:    approval.OutcomeAllowOnce,
		Reason:     approval.ReasonUser,
		ResolvedAt: r.clock().UTC(),
	}, nil
}

func canonicalProbeWorkspace(workspace string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(workspace))
	if err != nil || strings.TrimSpace(workspace) == "" {
		return "", capabilityprofile.ErrInvalidRuntime
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", capabilityprofile.ErrInvalidRuntime
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", capabilityprofile.ErrInvalidRuntime
	}
	return filepath.Clean(canonical), nil
}

func canonicalFixtureMutationPath(workspace, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) {
		return "", errProbeApprovalDenied
	}
	candidate := filepath.Clean(filepath.Join(workspace, value))
	relative, err := filepath.Rel(workspace, candidate)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errProbeApprovalDenied
	}
	info, err := os.Lstat(candidate)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errProbeApprovalDenied
	}
	canonical, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", errProbeApprovalDenied
	}
	relative, err = filepath.Rel(workspace, canonical)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errProbeApprovalDenied
	}
	return filepath.Clean(canonical), nil
}

func approvalDraftPath(redacted string) (string, error) {
	var payload struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal([]byte(redacted), &payload) != nil || strings.TrimSpace(payload.FilePath) == "" {
		return "", errProbeApprovalDenied
	}
	return payload.FilePath, nil
}

func probeToolInputDigest(tool string, input json.RawMessage) string {
	sum := sha256.Sum256(append([]byte(tool+"\x00"), input...))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// probeProcessDenyRunner preserves the delegate's truthful capability report
// for RunOptions validation but never starts a model-issued process. Profiling
// fixtures do not require Bash, Git, package managers, or other process tools.
type probeProcessDenyRunner struct {
	delegate sandbox.Runner
}

func (r *probeProcessDenyRunner) Name() string { return "capability-profile-process-deny" }

func (r *probeProcessDenyRunner) Capabilities() sandbox.Capabilities {
	if r == nil || r.delegate == nil {
		return sandbox.Capabilities{}
	}
	return r.delegate.Capabilities()
}

func (r *probeProcessDenyRunner) Run(_ context.Context, policy sandbox.Policy, _ sandbox.CommandSpec) (sandbox.Result, sandbox.Report) {
	result := sandbox.Result{
		Started: false, ExitCode: sandbox.ExitNotStarted,
		Err: fmt.Errorf("model-issued process tools are disabled during capability profiling"),
	}
	return result, sandbox.Report{
		Runner: r.Name(), RequestedEnforcement: policy.Enforcement,
		Capabilities: r.Capabilities(), Started: false,
		Failure: sandbox.FailureCommandInvalid,
		Detail:  "model-issued process tools are disabled during capability profiling",
	}
}
