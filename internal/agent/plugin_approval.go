package agent

import (
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/executionpolicy"
)

const (
	pluginApprovalExecutionProtocol       = "corelay.plugin-approval.v1"
	legacyPluginApprovalExecutionProtocol = "aniclew.plugin-approval.v1"
)

type pluginApprovalProof struct {
	ApprovalID              string                   `json:"approval_id"`
	SessionID               string                   `json:"session_id"`
	RunID                   string                   `json:"run_id"`
	ToolCallID              string                   `json:"tool_call_id"`
	ToolName                string                   `json:"tool_name"`
	ExecutorID              string                   `json:"executor_id"`
	ApprovalSource          approval.ApprovalSource  `json:"approval_source"`
	ExecutionPolicy         executionpolicy.Snapshot `json:"execution_policy"`
	ExecutionPolicyRevision uint64                   `json:"execution_policy_revision"`
	FullSelectionRevision   uint64                   `json:"full_selection_revision"`
	InputDigest             string                   `json:"input_digest"`
	ExpiresAt               int64                    `json:"expires_at"`
	Signature               string                   `json:"signature"`
}

type pluginApprovalExecutionEnvelope struct {
	Protocol string              `json:"protocol"`
	Approval pluginApprovalProof `json:"approval"`
	Input    json.RawMessage     `json:"input"`
}

var pluginApprovalProofKey struct {
	once sync.Once
	key  []byte
	err  error
}

var usedPluginApprovals sync.Map

func mintPluginApproval(
	pending approval.Pending,
	toolCallID string,
	input json.RawMessage,
	policy ExecutionPolicySnapshot,
) (pluginApprovalProof, error) {
	if err := validateApprovalProofMetadata(
		pending,
		toolCallID,
		pending.ToolName,
		pending.ExecutorID,
		input,
		policy,
	); err != nil {
		return pluginApprovalProof{}, err
	}
	if !strings.HasPrefix(strings.TrimSpace(pending.ExecutorID), "plugin:sha256:") {
		return pluginApprovalProof{}, errors.New("plugin approval executor identity is invalid")
	}
	proof := pluginApprovalProof{
		ApprovalID:              strings.TrimSpace(pending.ID),
		SessionID:               strings.TrimSpace(pending.SessionID),
		RunID:                   strings.TrimSpace(pending.RunID),
		ToolCallID:              strings.TrimSpace(toolCallID),
		ToolName:                strings.TrimSpace(pending.ToolName),
		ExecutorID:              strings.TrimSpace(pending.ExecutorID),
		ApprovalSource:          pending.ApprovalSource,
		ExecutionPolicy:         policy,
		ExecutionPolicyRevision: policy.Revision,
		FullSelectionRevision:   policy.FullSelectionRevision,
		InputDigest:             pending.InputDigest,
		ExpiresAt:               pending.ExpiresAt.UnixNano(),
	}
	signature, err := signPluginApproval(proof)
	if err != nil {
		return pluginApprovalProof{}, err
	}
	proof.Signature = signature
	return proof, nil
}

func validatePluginApprovalBound(
	proof pluginApprovalProof,
	toolName string,
	executorID string,
	input json.RawMessage,
	expectedSessionID string,
	expectedRunID string,
	expectedToolCallID string,
	policy *ExecutionPolicySnapshot,
) error {
	if policy == nil {
		return errors.New("plugin approval execution policy is not configured")
	}
	if err := executionpolicy.ValidateSnapshot(*policy); err != nil {
		return fmt.Errorf("plugin approval execution policy is invalid: %w", err)
	}
	if strings.TrimSpace(expectedSessionID) == "" || strings.TrimSpace(expectedRunID) == "" || strings.TrimSpace(expectedToolCallID) == "" {
		return errors.New("plugin approval execution binding is not configured")
	}
	if proof.SessionID != expectedSessionID || proof.RunID != expectedRunID || proof.ToolCallID != expectedToolCallID {
		return errors.New("plugin approval is not bound to the active session, run, and tool call")
	}
	if proof.ToolName != toolName || proof.ExecutorID != executorID ||
		proof.InputDigest != approvalToolInputDigest(toolName, input) {
		return errors.New("plugin approval is not bound to this execution")
	}
	if proof.ExecutionPolicy != *policy ||
		proof.ExecutionPolicyRevision != policy.Revision ||
		proof.FullSelectionRevision != policy.FullSelectionRevision {
		return errors.New("plugin approval is not bound to the active execution policy")
	}
	return consumePluginApprovalProof(proof)
}

func consumePluginApprovalProof(proof pluginApprovalProof) error {
	if proof.ApprovalID == "" || proof.Signature == "" {
		return errors.New("plugin approval metadata is incomplete")
	}
	if proof.ExpiresAt <= 0 || !time.Unix(0, proof.ExpiresAt).After(time.Now()) {
		return errors.New("plugin approval proof expired")
	}
	expected, err := signPluginApproval(proof)
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(proof.Signature)
	if err != nil || !hmac.Equal(provided, mustDecodeHex(expected)) {
		return errors.New("plugin approval signature does not match")
	}
	cleanupExpiredPluginApprovals(time.Now())
	if _, reused := usedPluginApprovals.LoadOrStore(proof.Signature, proof.ExpiresAt); reused {
		return errors.New("plugin approval proof was already consumed")
	}
	return nil
}

func bindPluginApprovalExecutionInput(input json.RawMessage, proof pluginApprovalProof) (json.RawMessage, error) {
	if strings.TrimSpace(proof.Signature) == "" {
		return nil, errors.New("plugin approval proof is empty")
	}
	encoded, err := json.Marshal(pluginApprovalExecutionEnvelope{
		Protocol: pluginApprovalExecutionProtocol,
		Approval: proof,
		Input:    append(json.RawMessage(nil), input...),
	})
	if err != nil {
		return nil, fmt.Errorf("encode plugin approval execution: %w", err)
	}
	return encoded, nil
}

func unwrapPluginApprovalExecutionInput(input json.RawMessage) (json.RawMessage, pluginApprovalProof, bool, error) {
	var probe struct {
		Protocol string `json:"protocol"`
	}
	if json.Unmarshal(input, &probe) != nil ||
		!renamedProtocolMatches(probe.Protocol, pluginApprovalExecutionProtocol, legacyPluginApprovalExecutionProtocol) {
		return input, pluginApprovalProof{}, false, nil
	}
	var envelope pluginApprovalExecutionEnvelope
	if err := json.Unmarshal(input, &envelope); err != nil {
		return nil, pluginApprovalProof{}, true, fmt.Errorf("decode plugin approval execution: %w", err)
	}
	if len(envelope.Input) == 0 || strings.TrimSpace(envelope.Approval.Signature) == "" {
		return nil, pluginApprovalProof{}, true, errors.New("plugin approval execution envelope is incomplete")
	}
	return append(json.RawMessage(nil), envelope.Input...), envelope.Approval, true, nil
}

func approvalToolInputDigest(toolName string, input json.RawMessage) string {
	value := make([]byte, 0, len(toolName)+len(input)+1)
	value = append(value, toolName...)
	value = append(value, 0)
	value = append(value, input...)
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validateApprovalProofMetadata(
	pending approval.Pending,
	toolCallID string,
	toolName string,
	executorID string,
	input json.RawMessage,
	policy ExecutionPolicySnapshot,
) error {
	if err := executionpolicy.ValidateSnapshot(policy); err != nil {
		return fmt.Errorf("approval execution policy is invalid: %w", err)
	}
	if strings.TrimSpace(pending.ID) == "" || strings.TrimSpace(pending.SessionID) == "" ||
		strings.TrimSpace(pending.RunID) == "" || strings.TrimSpace(toolCallID) == "" ||
		strings.TrimSpace(toolName) == "" || strings.TrimSpace(executorID) == "" ||
		strings.TrimSpace(pending.InputDigest) == "" || pending.ExpiresAt.IsZero() || !pending.ExpiresAt.After(time.Now()) {
		return errors.New("approval metadata is incomplete or expired")
	}
	if pending.ToolCallID != toolCallID || pending.ToolName != toolName || pending.ExecutorID != executorID ||
		pending.InputDigest != approvalToolInputDigest(toolName, input) ||
		pending.ExecutionPolicyRevision != policy.Revision || pending.FullSelectionRevision != policy.FullSelectionRevision {
		return errors.New("approval request is not bound to this tool call and execution policy")
	}
	if err := validateApprovalProofSource(pending.ApprovalSource, policy); err != nil {
		return err
	}
	return nil
}

func validateApprovalProofSource(source approval.ApprovalSource, policy ExecutionPolicySnapshot) error {
	switch source {
	case approval.ApprovalSourceUser:
		if policy.Mode == ExecutionModeFull {
			return errors.New("full-mode approval must come from a user-selected full grant")
		}
	case approval.ApprovalSourceUserSelectedFull:
		if policy.Mode != ExecutionModeFull || policy.FullSelectionRevision == 0 {
			return errors.New("full-mode approval provenance is invalid")
		}
		switch policy.Source {
		case executionpolicy.SourceUserSelected:
			if policy.Revision == 0 || policy.FullSelectionRevision != policy.Revision {
				return errors.New("full-mode approval provenance is invalid")
			}
		case executionpolicy.SourceInherited, executionpolicy.SourceChildRestriction:
			if policy.ParentRevision == 0 || policy.ParentRevision != policy.Revision {
				return errors.New("child full-mode approval is not bound to its parent selection")
			}
		default:
			return errors.New("full-mode approval provenance is invalid")
		}
	default:
		return errors.New("approval source is not an explicit user authorization")
	}
	return nil
}

func signPluginApproval(proof pluginApprovalProof) (string, error) {
	key, err := ensurePluginApprovalProofKey()
	if err != nil {
		return "", err
	}
	proof.Signature = ""
	encoded, err := json.Marshal(proof)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(encoded)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func ensurePluginApprovalProofKey() ([]byte, error) {
	pluginApprovalProofKey.once.Do(func() {
		key := make([]byte, 32)
		if _, err := cryptorand.Read(key); err != nil {
			pluginApprovalProofKey.err = fmt.Errorf("create plugin approval proof key: %w", err)
			return
		}
		pluginApprovalProofKey.key = key
	})
	if pluginApprovalProofKey.err != nil {
		return nil, pluginApprovalProofKey.err
	}
	if len(pluginApprovalProofKey.key) != 32 {
		return nil, errors.New("plugin approval proof key is unavailable")
	}
	return pluginApprovalProofKey.key, nil
}

func cleanupExpiredPluginApprovals(now time.Time) {
	cutoff := now.UnixNano()
	usedPluginApprovals.Range(func(key, value any) bool {
		expiresAt, ok := value.(int64)
		if !ok || expiresAt <= cutoff {
			usedPluginApprovals.Delete(key)
		}
		return true
	})
}
