package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
)

func TestPluginFullModeApprovalProofPreservesBrokerCallBinding(t *testing.T) {
	const (
		sessionID  = "full-proof-session"
		runID      = "full-proof-run"
		callID     = "full-proof-call"
		toolName   = "plugin_full"
		executorID = "plugin:sha256:proof-fixture"
	)
	input := json.RawMessage(`{"value":"approved"}`)
	policy, err := ResolveExecutionPolicy(ExecutionPolicyRequest{
		Mode: ExecutionModeFull, Revision: 41,
	}, "", ExecutionPolicySnapshot{}.RuntimeCapabilities)
	if err != nil {
		t.Fatalf("ResolveExecutionPolicy: %v", err)
	}
	draft := approval.Draft{
		SessionID:               sessionID,
		SessionRevision:         3,
		RunID:                   runID,
		ToolCallID:              callID,
		ToolName:                toolName,
		ExecutorID:              executorID,
		RedactedInput:           `{"input":"omitted"}`,
		InputDigest:             approvalToolInputDigest(toolName, input),
		ExecutionPolicyRevision: policy.Revision,
		FullSelectionRevision:   policy.FullSelectionRevision,
	}
	broker := approval.NewBroker(time.Minute)
	defer broker.Shutdown()
	pending, err := broker.IssueFullModeGrant(draft, policy)
	if err != nil {
		t.Fatalf("IssueFullModeGrant: %v", err)
	}
	if pending.ApprovalSource != approval.ApprovalSourceUserSelectedFull {
		t.Fatalf("approval source = %q", pending.ApprovalSource)
	}
	proof, err := mintPluginApproval(pending, callID, input, policy)
	if err != nil {
		t.Fatalf("mintPluginApproval: %v", err)
	}
	if err := broker.ConsumeFullModeGrant(pending, draft, policy); err != nil {
		t.Fatalf("ConsumeFullModeGrant: %v", err)
	}

	if err := validatePluginApprovalBound(proof, toolName, executorID, input, sessionID, runID, "changed-call", &policy); err == nil {
		t.Fatal("proof accepted a different tool call ID")
	}
	if err := validatePluginApprovalBound(proof, toolName, "plugin:sha256:other", input, sessionID, runID, callID, &policy); err == nil {
		t.Fatal("proof accepted a different executor identity")
	}
	if err := validatePluginApprovalBound(proof, toolName, executorID, json.RawMessage(`{"value":"changed"}`), sessionID, runID, callID, &policy); err == nil {
		t.Fatal("proof accepted changed input")
	}
	changedPolicy := policy
	changedPolicy.Revision++
	changedPolicy.FullSelectionRevision++
	if err := validatePluginApprovalBound(proof, toolName, executorID, input, sessionID, runID, callID, &changedPolicy); err == nil {
		t.Fatal("proof accepted a changed execution policy")
	}
	if err := validatePluginApprovalBound(proof, toolName, executorID, input, sessionID, runID, callID, &policy); err != nil {
		t.Fatalf("exact proof rejected: %v", err)
	}
	if err := validatePluginApprovalBound(proof, toolName, executorID, input, sessionID, runID, callID, &policy); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("replayed proof error = %v", err)
	}
}

func TestPluginInheritedFullGrantProofRetainsParentSelectionRevision(t *testing.T) {
	const (
		sessionID  = "inherited-full-session"
		runID      = "inherited-full-run"
		callID     = "inherited-full-call"
		toolName   = "plugin_inherited_full"
		executorID = "plugin:sha256:inherited-proof-fixture"
	)
	parent, err := ResolveExecutionPolicy(ExecutionPolicyRequest{
		Mode: ExecutionModeFull, Revision: 53,
	}, "", ExecutionPolicySnapshot{}.RuntimeCapabilities)
	if err != nil {
		t.Fatalf("ResolveExecutionPolicy: %v", err)
	}
	policy, err := ResolveChildExecutionPolicy(parent, "", parent.RuntimeCapabilities)
	if err != nil {
		t.Fatalf("ResolveChildExecutionPolicy: %v", err)
	}
	input := json.RawMessage(`{"value":"inherited"}`)
	draft := approval.Draft{
		SessionID:               sessionID,
		SessionRevision:         4,
		RunID:                   runID,
		ToolCallID:              callID,
		ToolName:                toolName,
		ExecutorID:              executorID,
		InputDigest:             approvalToolInputDigest(toolName, input),
		ExecutionPolicyRevision: policy.Revision,
		FullSelectionRevision:   policy.FullSelectionRevision,
	}
	broker := approval.NewBroker(time.Minute)
	defer broker.Shutdown()
	pending, err := broker.IssueFullModeGrant(draft, policy)
	if err != nil {
		t.Fatalf("IssueFullModeGrant(child) = %v", err)
	}
	proof, err := mintPluginApproval(pending, callID, input, policy)
	if err != nil {
		t.Fatalf("mintPluginApproval(child): %v", err)
	}
	if err := broker.ConsumeFullModeGrant(pending, draft, policy); err != nil {
		t.Fatalf("ConsumeFullModeGrant(child): %v", err)
	}
	if proof.FullSelectionRevision != parent.FullSelectionRevision ||
		proof.ExecutionPolicyRevision != policy.Revision {
		t.Fatalf("proof lost parent selection provenance: %#v", proof)
	}
	if err := validatePluginApprovalBound(proof, toolName, executorID, input, sessionID, runID, callID, &policy); err != nil {
		t.Fatalf("child-bound proof rejected: %v", err)
	}
}

func TestApprovalProofRejectsSourcePolicyMismatch(t *testing.T) {
	input := json.RawMessage(`{"text":"type safely"}`)
	policy := hostTestExecutionPolicy()
	pending := approval.Pending{
		ID:                      "source-proof",
		SessionID:               "source-session",
		RunID:                   "source-run",
		ToolCallID:              "source-call",
		ToolName:                "TypeText",
		ExecutorID:              "builtin:TypeText",
		InputDigest:             approvalToolInputDigest("TypeText", input),
		ExecutionPolicyRevision: policy.Revision,
		ApprovalSource:          approval.ApprovalSource("automatic-rule"),
		ExpiresAt:               time.Now().Add(time.Minute),
	}
	if _, err := mintHostInteractionApproval(pending, pending.ToolCallID, input, policy); err == nil {
		t.Fatal("non-user approval source minted a host proof")
	}
}
