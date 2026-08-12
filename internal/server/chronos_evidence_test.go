package server

import (
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/agent"
)

func TestChronosVerificationFromRunModeDone(t *testing.T) {
	for _, test := range []struct {
		name       string
		data       any
		wantStatus string
		wantText   string
	}{
		{
			name: "verified completion",
			data: map[string]interface{}{"runMode": agent.RunModeSnapshot{
				Name: "chronos", StopReason: "complete",
				State: agent.ChronosState{Phase: "complete", VerifyResults: []string{"all checks passed"}},
			}},
			wantStatus: "passed", wantText: "all checks passed",
		},
		{
			name: "max cycles is failure",
			data: map[string]interface{}{"stopReason": "max_cycles", "runMode": agent.RunModeSnapshot{
				Name: "chronos", StopReason: "max_cycles",
				State: agent.ChronosState{Phase: "failed", Findings: []string{"still failing"}},
			}},
			wantStatus: "failed", wantText: "cycle limit",
		},
		{
			name: "find phase completion has no fabricated verification",
			data: map[string]interface{}{"runMode": agent.RunModeSnapshot{
				Name: "chronos", StopReason: "complete", State: agent.ChronosState{Phase: "complete"},
			}},
			wantStatus: "not-run", wantText: "completed",
		},
		{
			name: "completion contract blocked",
			data: map[string]interface{}{
				"terminalState": "blocked", "completionStatus": "blocked", "completionBlocked": 1,
				"runMode": agent.RunModeSnapshot{Name: "chronos", StopReason: "complete", State: agent.ChronosState{Phase: "complete"}},
			},
			wantStatus: "failed", wantText: "blocked",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := chronosVerificationFromDone(test.data, "go test ./...")
			if result.Status != test.wantStatus || !strings.Contains(result.Summary, test.wantText) {
				t.Fatalf("result = %#v, want status %q containing %q", result, test.wantStatus, test.wantText)
			}
		})
	}
}
