package server

import (
	"fmt"
	"strings"

	"github.com/aniclew/aniclew/internal/agent"
	"github.com/aniclew/aniclew/internal/workstream"
)

func chronosVerificationFromDone(data any, verifyCommand string) workstream.VerificationResult {
	result := workstream.VerificationResult{
		Status:  "not-run",
		Source:  "chronos",
		Summary: "chronos run completed",
	}

	state, ok := data.(agent.ChronosState)
	if !ok {
		if ptr, ptrOK := data.(*agent.ChronosState); ptrOK && ptr != nil {
			state = *ptr
			ok = true
		}
	}
	if !ok || len(state.VerifyResults) == 0 {
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
