package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func projectProbeFailure(code string) observedAgentProbe {
	return lifecycleFailure(code)
}

// project selects persisted sessions A/B/A, reopening the store between every
// selection. Only that session's canonical workspace and transcript enter a run.
func (p lifecycleProbe) project(ctx context.Context) observedAgentProbe {
	started := time.Now()
	storePath, err := os.MkdirTemp("", "corelay-project-sessions-")
	if err != nil {
		return projectProbeFailure("project_store_unavailable")
	}
	defer os.RemoveAll(storePath)
	var ids, roots, secrets [2]string
	store := agent.NewSessionStore(storePath)
	for i, name := range []string{"project-a", "project-b"} {
		roots[i] = filepath.Join(p.execution.WorkspaceRoot, name)
		if err := os.MkdirAll(roots[i], 0700); err != nil {
			return projectProbeFailure("project_workspace_unavailable")
		}
		nonce := make([]byte, 24)
		if _, err := rand.Read(nonce); err != nil {
			return projectProbeFailure("project_marker_unavailable")
		}
		secrets[i] = "PROJECT_" + hex.EncodeToString(nonce)
		if err := os.WriteFile(filepath.Join(roots[i], "README.md"), []byte(secrets[i]+"\n"), 0600); err != nil {
			return projectProbeFailure("project_workspace_unavailable")
		}
		session := agent.Session{Workspace: roots[i], Provider: p.executor.provider.Name(), Model: p.executor.model, Messages: []agent.SessionMessage{}}
		if err := store.SaveExpected(&session, 0); err != nil {
			return projectProbeFailure("project_session_create_failed")
		}
		// The store canonicalizes separators, case and resolved workspace paths.
		// Subsequent selection compares canonical persisted identities.
		roots[i] = session.Workspace
		ids[i] = session.ID
	}
	traces := []string{}
	var final observedAgentProbe
	var maxContext, maxTools, retries int
	var malformed, recovered bool
	for _, index := range []int{0, 1, 0} {
		if ctx.Err() != nil {
			return projectProbeFailure("project_canceled")
		}
		store = agent.NewSessionStore(storePath)
		selected, err := store.Get(ids[index])
		if err != nil || selected.Workspace != roots[index] {
			return projectProbeFailure("project_session_workspace_mismatch")
		}
		otherBefore, err := store.Get(ids[1-index])
		if err != nil {
			return projectProbeFailure("project_other_session_missing")
		}
		otherBytes, _ := json.Marshal(otherBefore)
		messages := make([]types.Message, 0, len(selected.Messages)+1)
		for _, message := range selected.Messages {
			if strings.Contains(message.Content, secrets[1-index]) {
				return projectProbeFailure("project_transcript_leak")
			}
			content, _ := json.Marshal(message.Content)
			messages = append(messages, types.Message{Role: message.Role, Content: content})
		}
		prompt := "Read README.md from the selected workspace using Read. Reply with exactly its full trimmed contents, and nothing else. Do not rely on earlier answers."
		content, _ := json.Marshal(prompt)
		messages = append(messages, types.Message{Role: "user", Content: content})
		selected.Messages = append(selected.Messages, agent.SessionMessage{Role: "user", Content: prompt, Timestamp: time.Now().UTC()})
		if err := store.SaveExpected(selected, selected.Revision); err != nil {
			return projectProbeFailure("project_append_failed")
		}
		options := p.options
		options.SessionID = selected.ID
		options.DurableSessionID = selected.ID
		options.SessionRevision = selected.Revision
		options.SandboxPolicy.Workspace = selected.Workspace
		result := p.run(ctx, selected.Workspace, messages, agentProbeFixture{marker: secrets[index]}, options)
		if !result.value.Success || result.transportFailed || result.successfulReads < 1 || strings.TrimSpace(result.finalText) != secrets[index] {
			result.value.Success = false
			result.value.FalseDone = !result.transportFailed
			result.transportFailed = true
			result.failureCode = "project_run_wrong_workspace_or_answer"
			return result
		}
		selected.Messages = append(selected.Messages, agent.SessionMessage{Role: "assistant", Content: result.finalText, Timestamp: time.Now().UTC()})
		if err := store.SaveExpected(selected, selected.Revision); err != nil {
			return projectProbeFailure("project_result_save_failed")
		}
		otherAfter, err := agent.NewSessionStore(storePath).Get(ids[1-index])
		if err != nil {
			return projectProbeFailure("project_other_session_missing")
		}
		afterBytes, _ := json.Marshal(otherAfter)
		if string(otherBytes) != string(afterBytes) {
			return projectProbeFailure("project_unselected_session_mutated")
		}
		traces = append(traces, result.value.TraceDigest)
		maxContext = max(maxContext, result.value.ContextTokens)
		maxTools = max(maxTools, result.value.ToolCount)
		retries += result.value.Retries
		malformed = malformed || result.value.Malformed
		recovered = recovered || result.value.Recovered
		final = result
	}
	store = agent.NewSessionStore(storePath)
	for i, wantMessages := range []int{4, 2} {
		contents, err := os.ReadFile(filepath.Join(roots[i], "README.md"))
		if err != nil || string(contents) != secrets[i]+"\n" {
			return projectProbeFailure("project_source_mutated")
		}
		session, err := store.Get(ids[i])
		if err != nil || len(session.Messages) != wantMessages {
			return projectProbeFailure("project_resume_transcript_mismatch")
		}
		for _, message := range session.Messages {
			if strings.Contains(message.Content, secrets[1-i]) {
				return projectProbeFailure("project_transcript_leak")
			}
		}
	}
	final.value.TraceDigest = digestJSON(traces)
	final.value.ArtifactDigest = digestJSON(struct {
		Sessions [2]string
		Turns    []int
	}{ids, []int{2, 1}})
	final.value.Latency = time.Since(started)
	final.value.ContextTokens = maxContext
	final.value.ToolCount = maxTools
	final.value.Retries = retries
	final.value.Malformed = malformed
	final.value.Recovered = recovered
	return final
}
