package main

import (
	"os"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
)

func TestLifecycleSnapshotsCannotPassWithoutLifecycleExecution(t *testing.T) {
	for _, probeCase := range capabilityprofile.DefaultProbePlan().Cases() {
		if !isLifecycleProbe(probeCase.Category) {
			continue
		}
		t.Run(string(probeCase.Category), func(t *testing.T) {
			execution := capabilityprofile.ProbeExecution{Case: probeCase, Attempt: 1, WorkspaceRoot: t.TempDir()}
			fixture, err := prepareAgentProbeFixture(execution)
			if err != nil {
				t.Fatal(err)
			}
			for _, artifact := range fixture.artifactFiles {
				if err := os.WriteFile(artifact.path, artifact.expected, 0600); err != nil {
					t.Fatal(err)
				}
			}
			events := make(chan agent.Event, 6)
			for _, name := range []string{"Read", "Read", "Write"} {
				events <- agent.Event{Type: "tool_result", Data: map[string]any{"name": name, "executed": true, "isError": false}}
			}
			events <- agent.Event{Type: "text", Data: fixture.marker}
			events <- agent.Event{Type: "done"}
			close(events)
			result := observeAgentProbe(events, fixture, execution, time.Now())
			if result.value.Success || !result.value.FalseDone {
				t.Fatal("snapshot-only lifecycle trace must not pass")
			}
		})
	}
}
