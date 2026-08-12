package agent

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	maxRunModeNameBytes         = 64
	maxRunModePromptBytes       = 32 << 10
	maxRunModeStatusCount       = 16
	maxRunModeStatusBytes       = 2 << 10
	maxRunModeTerminalTextBytes = 8 << 10
	maxRunModeStopReasonBytes   = 128
)

func resolveRunMode(mode RunMode) (string, error) {
	if mode == nil {
		return "", nil
	}
	name := strings.TrimSpace(mode.Name())
	if name == "" || len(name) > maxRunModeNameBytes || !utf8.ValidString(name) {
		return "", fmt.Errorf("run mode name is invalid")
	}
	suffix := strings.TrimSpace(mode.SystemPromptSuffix())
	if len(suffix) > maxRunModePromptBytes || !utf8.ValidString(suffix) {
		return "", fmt.Errorf("run mode system prompt is invalid")
	}
	return suffix, nil
}

func validateRunModeDirective(directive RunModeDirective, start bool) error {
	if start && !directive.Continue {
		return fmt.Errorf("run mode start must continue")
	}
	if directive.Continue && !start && strings.TrimSpace(directive.UserPrompt) == "" {
		return fmt.Errorf("continuing run mode directive requires a user prompt")
	}
	if !directive.Continue && strings.TrimSpace(directive.UserPrompt) != "" {
		return fmt.Errorf("terminal run mode directive cannot include a user prompt")
	}
	if len(directive.UserPrompt) > maxRunModePromptBytes || !utf8.ValidString(directive.UserPrompt) {
		return fmt.Errorf("run mode user prompt is invalid")
	}
	if len(directive.Status) > maxRunModeStatusCount {
		return fmt.Errorf("run mode emitted too many status messages")
	}
	for _, status := range directive.Status {
		if len(status) > maxRunModeStatusBytes || !utf8.ValidString(status) {
			return fmt.Errorf("run mode status is invalid")
		}
	}
	if directive.Continue && (directive.TerminalText != "" || directive.StopReason != "") {
		return fmt.Errorf("continuing run mode directive contains terminal data")
	}
	if len(directive.TerminalText) > maxRunModeTerminalTextBytes || !utf8.ValidString(directive.TerminalText) {
		return fmt.Errorf("run mode terminal text is invalid")
	}
	if len(directive.StopReason) > maxRunModeStopReasonBytes || !utf8.ValidString(directive.StopReason) {
		return fmt.Errorf("run mode stop reason is invalid")
	}
	return nil
}

func appendRunModeDirective(messages []types.Message, directive RunModeDirective) []types.Message {
	if strings.TrimSpace(directive.UserPrompt) == "" {
		return messages
	}
	return append(messages, types.Message{Role: "user", Content: mustJSON(directive.UserPrompt)})
}

func effectiveRunIterationLimit(profile harness.HarnessProfile, override int) (int, error) {
	profileLimit := profile.MaxIterations()
	if override == 0 {
		return profileLimit, nil
	}
	if override < 1 || override > harness.MaxIterationsLimit {
		return 0, fmt.Errorf("iteration limit must be between 1 and %d", harness.MaxIterationsLimit)
	}
	if profileLimit > 0 && profileLimit < override {
		return profileLimit, nil
	}
	return override, nil
}

func runModePlanStep(mode RunMode, fallback string) string {
	if mode == nil {
		return fallback
	}
	step := strings.TrimSpace(mode.CurrentStep())
	if step == "" || len(step) > maxRunModeNameBytes || !utf8.ValidString(step) {
		return fallback
	}
	return step
}

func validateRunModeSnapshot(mode RunMode, snapshot RunModeSnapshot) error {
	if mode == nil {
		return fmt.Errorf("run mode snapshot has no owner")
	}
	if strings.TrimSpace(snapshot.Name) != strings.TrimSpace(mode.Name()) ||
		len(snapshot.Name) > maxRunModeNameBytes || !utf8.ValidString(snapshot.Name) {
		return fmt.Errorf("run mode snapshot name is invalid")
	}
	if len(snapshot.StopReason) > maxRunModeStopReasonBytes || !utf8.ValidString(snapshot.StopReason) {
		return fmt.Errorf("run mode snapshot stop reason is invalid")
	}
	return nil
}

func addRunModeTokenEstimate(total *int, estimate int) {
	if total == nil || estimate <= 0 {
		return
	}
	const maxInt = int(^uint(0) >> 1)
	if *total > maxInt-estimate {
		*total = maxInt
		return
	}
	*total += estimate
}
