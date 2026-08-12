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
)

const (
	pluginApprovalExecutionProtocol       = "corelay.plugin-approval.v1"
	legacyPluginApprovalExecutionProtocol = "aniclew.plugin-approval.v1"
)

type pluginApprovalProof struct {
	ApprovalID  string `json:"approval_id"`
	SessionID   string `json:"session_id"`
	RunID       string `json:"run_id"`
	ToolName    string `json:"tool_name"`
	ExecutorID  string `json:"executor_id"`
	InputDigest string `json:"input_digest"`
	ExpiresAt   int64  `json:"expires_at"`
	Signature   string `json:"signature"`
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
	approvalID string,
	sessionID string,
	runID string,
	toolName string,
	executorID string,
	input json.RawMessage,
	expiresAt time.Time,
) (pluginApprovalProof, error) {
	proof := pluginApprovalProof{
		ApprovalID:  strings.TrimSpace(approvalID),
		SessionID:   strings.TrimSpace(sessionID),
		RunID:       strings.TrimSpace(runID),
		ToolName:    strings.TrimSpace(toolName),
		ExecutorID:  strings.TrimSpace(executorID),
		InputDigest: pluginApprovalInputDigest(toolName, executorID, input),
		ExpiresAt:   expiresAt.UnixNano(),
	}
	if proof.ApprovalID == "" || proof.SessionID == "" || proof.RunID == "" ||
		proof.ToolName == "" || !strings.HasPrefix(proof.ExecutorID, "plugin:sha256:") ||
		expiresAt.IsZero() || !expiresAt.After(time.Now()) {
		return pluginApprovalProof{}, errors.New("plugin approval metadata is incomplete")
	}
	signature, err := signPluginApproval(proof)
	if err != nil {
		return pluginApprovalProof{}, err
	}
	proof.Signature = signature
	return proof, nil
}

func validatePluginApproval(
	proof pluginApprovalProof,
	toolName string,
	executorID string,
	input json.RawMessage,
	expectedSessionID string,
	expectedRunID string,
) error {
	if strings.TrimSpace(expectedSessionID) == "" || strings.TrimSpace(expectedRunID) == "" {
		return errors.New("plugin approval execution binding is not configured")
	}
	if proof.SessionID != expectedSessionID || proof.RunID != expectedRunID {
		return errors.New("plugin approval is not bound to the active session and run")
	}
	if proof.ToolName != toolName || proof.ExecutorID != executorID ||
		proof.InputDigest != pluginApprovalInputDigest(toolName, executorID, input) {
		return errors.New("plugin approval is not bound to this execution")
	}
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

func pluginApprovalInputDigest(toolName, executorID string, input json.RawMessage) string {
	value := make([]byte, 0, len(toolName)+len(executorID)+len(input)+2)
	value = append(value, toolName...)
	value = append(value, 0)
	value = append(value, executorID...)
	value = append(value, 0)
	value = append(value, input...)
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
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
