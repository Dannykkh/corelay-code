package acpbridge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/acp"
	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestACPLoadRejectsWorkflowBindingWithoutMutation(t *testing.T) {
	for _, planned := range []bool{false, true} {
		fixture := newBackendFixture(t, scriptedRunner())
		session := &agent.Session{Workspace: fixture.workspace, Provider: "fake", Model: "model-a", WorkstreamID: "ws_test"}
		if planned {
			session.PlanID = "plan_test"
			session.PlanRevision = 1
			session.StageID = "stage_test"
		}
		if err := fixture.store.SaveExpected(session, 0); err != nil {
			t.Fatal(err)
		}
		_, err := fixture.backend.LoadSession(context.Background(), acp.LoadSessionRequest{SessionID: session.ID, CWD: fixture.workspace}, fixture.client)
		if err == nil || !strings.Contains(err.Error(), "workflow-bound sessions are unsupported") {
			t.Fatalf("LoadSession error = %v", err)
		}
		current, err := fixture.store.Get(session.ID)
		if err != nil || current.Revision != session.Revision || len(current.Messages) != 0 {
			t.Fatalf("load mutated session: %+v %v", current, err)
		}
		if len(fixture.backend.sessions) != 0 {
			t.Fatal("unsupported load created runtime session")
		}
	}
}

func TestACPPromptRejectsEachWorkflowBindingBeforeRunnerOrMutation(t *testing.T) {
	for name, bind := range map[string]func(*agent.Session){
		"workstream": func(s *agent.Session) { s.WorkstreamID = "ws_test" },
		"plan":       func(s *agent.Session) { s.PlanID = "plan_test" },
		"revision":   func(s *agent.Session) { s.PlanRevision = 1 },
		"stage":      func(s *agent.Session) { s.StageID = "stage_test" },
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			runner := RunnerFunc(func(context.Context, types.Provider, string, []types.Message, string, agent.RunOptions) (<-chan agent.Event, error) {
				calls++
				return nil, errors.New("unexpected runner")
			})
			fixture := newBackendFixture(t, runner)
			id := fixture.newSession(t)
			before, err := fixture.store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			fixture.backend.mu.Lock()
			bind(&fixture.backend.sessions[id].persisted)
			fixture.backend.mu.Unlock()
			_, err = fixture.backend.Prompt(context.Background(), acp.PromptRequest{SessionID: id, Prompt: []acp.ContentBlock{{Type: "text", Text: "run"}}}, fixture.client)
			if err == nil || !strings.Contains(err.Error(), "workflow-bound sessions are unsupported") {
				t.Fatalf("Prompt error = %v", err)
			}
			after, err := fixture.store.Get(id)
			if err != nil || after.Revision != before.Revision || len(after.Messages) != len(before.Messages) {
				t.Fatalf("prompt mutated session: %+v %v", after, err)
			}
			state := fixture.backend.sessions[id]
			if calls != 0 || state.promptStarting || state.cancel != nil || state.activeID != "" {
				t.Fatalf("unsupported prompt started execution: calls=%d", calls)
			}
		})
	}
}
