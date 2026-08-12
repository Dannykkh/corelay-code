package server

import (
	"fmt"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/workstream"
)

func chronosVerificationFromDone(data any, verifyCommand string) workstream.VerificationResult {
	result := workstream.VerificationResult{
		Status:  "not-run",
		Source:  "chronos",
		Summary: "chronos run completed",
	}
	if terminal, ok := agent.DecodeDurableRunTerminalMetadata(data); ok && terminal.BlocksSuccess() {
		result.Status = "failed"
		result.Summary = "chronos completion contract is blocked or incomplete"
		return result
	}

	state, stopReason, ok := chronosStateFromDone(data)
	if !ok {
		return result
	}
	if state.Phase == "failed" || stopReason == "max_cycles" {
		result.Status = "failed"
		result.Summary = "chronos stopped before verified completion"
		if stopReason == "max_cycles" {
			result.Summary = "chronos reached the configured cycle limit"
		}
		if len(state.Findings) > 0 {
			if last := trimEvidenceText(state.Findings[len(state.Findings)-1]); last != "" {
				result.Summary += ": " + last
			}
		}
		return result
	}
	if len(state.VerifyResults) == 0 {
		return result
	}

	last := trimEvidenceText(state.VerifyResults[len(state.VerifyResults)-1])
	result.Status = "passed"
	result.Summary = "chronos verification completed"
	if strings.TrimSpace(verifyCommand) != "" {
		result.Summary = fmt.Sprintf("%s via %s", result.Summary, verifyCommand)
	}
	if last != "" {
		result.Summary += ": " + last
	}
	return result
}

func chronosStateFromDone(data any) (agent.ChronosState, string, bool) {
	switch value := data.(type) {
	case agent.ChronosState:
		return value, "", true
	case *agent.ChronosState:
		if value != nil {
			return *value, "", true
		}
	case map[string]interface{}:
		stopReason, _ := value["stopReason"].(string)
		switch mode := value["runMode"].(type) {
		case agent.RunModeSnapshot:
			state, ok := chronosStateFromSnapshot(mode)
			if mode.StopReason != "" {
				stopReason = mode.StopReason
			}
			return state, stopReason, ok
		case *agent.RunModeSnapshot:
			if mode != nil {
				state, ok := chronosStateFromSnapshot(*mode)
				if mode.StopReason != "" {
					stopReason = mode.StopReason
				}
				return state, stopReason, ok
			}
		}
	}
	return agent.ChronosState{}, "", false
}

func chronosStateFromSnapshot(snapshot agent.RunModeSnapshot) (agent.ChronosState, bool) {
	if snapshot.Name != "chronos" {
		return agent.ChronosState{}, false
	}
	switch state := snapshot.State.(type) {
	case agent.ChronosState:
		return state, true
	case *agent.ChronosState:
		if state != nil {
			return *state, true
		}
	}
	return agent.ChronosState{}, false
}
