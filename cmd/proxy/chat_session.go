package main

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
)

type durableChatSessionSaver interface {
	SaveSession(context.Context, *agent.Session, *uint64) (sessionSaveResult, error)
}

func validateDurableChatWorkspace(session *agent.Session, requested string, explicit bool) error {
	if !explicit || session == nil || strings.TrimSpace(session.Workspace) == "" || strings.TrimSpace(requested) == "" {
		return nil
	}
	if sameDurableChatWorkspace(session.Workspace, requested) {
		return nil
	}
	return errors.New("workspace conflict: durable session belongs to " + session.Workspace + "; requested workspace is " + requested)
}

func sameDurableChatWorkspace(left, right string) bool {
	canonical := func(value string) (string, error) {
		absolute, err := filepath.Abs(strings.TrimSpace(value))
		if err != nil {
			return "", err
		}
		absolute = filepath.Clean(absolute)
		if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
			absolute = filepath.Clean(resolved)
		}
		return absolute, nil
	}
	a, errA := canonical(left)
	b, errB := canonical(right)
	if errA != nil || errB != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// saveDurableChatUserTurn is shared by the line client and TUI so both persist
// a user turn before starting the agent and bind the run to that exact revision.
func saveDurableChatUserTurn(
	ctx context.Context,
	backend durableChatSessionSaver,
	current *agent.Session,
	prompt, workDir, provider, model string,
) (*agent.Session, uint64, error) {
	if backend == nil {
		return nil, 0, errors.New("durable session backend is unavailable")
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, 0, errors.New("durable session prompt is empty")
	}
	session := cloneTUISession(current)
	if session == nil {
		session = &agent.Session{
			Workspace:       workDir,
			Title:           titleFromPrompt(prompt),
			Provider:        provider,
			Model:           model,
			LifecycleStatus: agent.SessionLifecycleActive,
		}
	}
	if strings.TrimSpace(session.Workspace) == "" {
		session.Workspace = workDir
	}
	session.Messages = append(session.Messages, agent.SessionMessage{
		Role: "user", Content: prompt, Timestamp: time.Now().UTC(),
	})
	session.Turns++
	var expected *uint64
	if session.ID != "" {
		revision := session.Revision
		expected = &revision
	}
	result, err := backend.SaveSession(ctx, session, expected)
	if err != nil {
		return nil, 0, err
	}
	session.ID = result.ID
	session.Version = result.Version
	session.Revision = result.Revision
	return session, result.Revision, nil
}
