package agent

import (
	"fmt"
	"strings"
	"time"
)

const chronosCompletionMarker = "[COMPLETE]"

type chronosRunMode struct {
	task    string
	workDir string
	config  ChronosConfig

	state             ChronosState
	started           bool
	finished          bool
	terminalDirective RunModeDirective
}

var _ RunMode = (*chronosRunMode)(nil)

func newChronosRunMode(task, workDir string, cfg ChronosConfig) RunMode {
	return &chronosRunMode{
		task:    strings.TrimSpace(task),
		workDir: strings.TrimSpace(workDir),
		config: ChronosConfig{
			MaxCycles:     cfg.MaxCycles,
			AutoFix:       cfg.AutoFix,
			VerifyCommand: strings.TrimSpace(cfg.VerifyCommand),
		},
	}
}

func (m *chronosRunMode) Name() string { return "chronos" }

func (m *chronosRunMode) SystemPromptSuffix() string {
	return fmt.Sprintf(`## Chronos Mode: FIND → FIX → VERIFY

Work through the task in bounded cycles:
1. FIND: analyze the current state and identify remaining work
2. FIX: make only the necessary changes
3. VERIFY: verify the result and report [COMPLETE] only when it is done

Task: %s
Maximum cycles: %d`, m.task, m.config.MaxCycles)
}

func (m *chronosRunMode) Start(now time.Time) (RunModeDirective, error) {
	if m.started {
		return RunModeDirective{}, fmt.Errorf("Chronos run mode has already started")
	}
	if m.config.MaxCycles <= 0 {
		return RunModeDirective{}, fmt.Errorf("Chronos MaxCycles must be greater than zero")
	}

	m.started = true
	m.state = ChronosState{
		Cycle:       1,
		Phase:       "find",
		StartedAt:   now,
		LastCycleAt: now,
	}
	return RunModeDirective{
		Continue:   true,
		UserPrompt: m.findPrompt(),
		Status: []string{
			fmt.Sprintf("Chronos: starting autonomous loop (max %d cycles)", m.config.MaxCycles),
			m.phaseStatus("FIND"),
		},
	}, nil
}

func (m *chronosRunMode) Advance(turn RunModeTurn) (RunModeDirective, error) {
	if !m.started {
		return RunModeDirective{}, fmt.Errorf("Chronos run mode has not started")
	}
	if m.finished {
		return cloneRunModeDirective(m.terminalDirective), nil
	}

	switch m.state.Phase {
	case "find":
		return m.advanceFind(turn), nil
	case "fix":
		return m.advanceFix(turn), nil
	case "verify":
		return m.advanceVerify(turn), nil
	default:
		return RunModeDirective{}, fmt.Errorf("Chronos run mode has invalid phase %q", m.state.Phase)
	}
}

func (m *chronosRunMode) CurrentStep() string { return m.state.Phase }

func (m *chronosRunMode) Snapshot(metrics RunModeMetrics) RunModeSnapshot {
	state := m.state
	state.TotalTools = metrics.TotalTools
	state.TotalTokens = metrics.EstimatedTokens
	state.Findings = append([]string(nil), m.state.Findings...)
	state.Fixes = append([]string(nil), m.state.Fixes...)
	state.VerifyResults = append([]string(nil), m.state.VerifyResults...)
	state.Sandbox = append([]SandboxExecutionRecord(nil), metrics.Sandbox...)
	return RunModeSnapshot{
		Name:       m.Name(),
		StopReason: m.terminalDirective.StopReason,
		State:      state,
	}
}

func (m *chronosRunMode) advanceFind(turn RunModeTurn) RunModeDirective {
	if chronosComplete(turn.Text) {
		m.state.Phase = "complete"
		return m.finish(RunModeDirective{
			Status: []string{fmt.Sprintf("Chronos: COMPLETE after %d cycles", m.state.Cycle)},
			TerminalText: fmt.Sprintf(
				"\n\n---\nChronos completed in %d cycles (%.0fs)\nFindings: %d, Fixes: %d\n",
				m.state.Cycle,
				m.elapsedSeconds(turn.Now),
				len(m.state.Findings),
				len(m.state.Fixes),
			),
			StopReason: "complete",
		})
	}

	m.state.Findings = append(m.state.Findings, truncateStr(turn.Text, 200))
	if !m.config.AutoFix {
		return m.finish(RunModeDirective{
			Status:     []string{"Chronos: findings reported (auto-fix disabled)"},
			StopReason: "auto_fix_disabled",
		})
	}

	m.state.Phase = "fix"
	return RunModeDirective{
		Continue:   true,
		UserPrompt: m.fixPrompt(),
		Status:     []string{m.phaseStatus("FIX")},
	}
}

func (m *chronosRunMode) advanceFix(turn RunModeTurn) RunModeDirective {
	m.state.Fixes = append(m.state.Fixes, truncateStr(turn.Text, 200))
	m.state.Phase = "verify"
	return RunModeDirective{
		Continue:   true,
		UserPrompt: m.verifyPrompt(),
		Status:     []string{m.phaseStatus("VERIFY")},
	}
}

func (m *chronosRunMode) advanceVerify(turn RunModeTurn) RunModeDirective {
	m.state.VerifyResults = append(m.state.VerifyResults, truncateStr(turn.Text, 200))
	if chronosComplete(turn.Text) {
		m.state.Phase = "complete"
		return m.finish(RunModeDirective{
			Status: []string{fmt.Sprintf("Chronos: VERIFIED & COMPLETE after %d cycles", m.state.Cycle)},
			TerminalText: fmt.Sprintf(
				"\n\n---\nChronos completed in %d cycles (%.0fs)\nFindings: %d, Fixes: %d, Verified\n",
				m.state.Cycle,
				m.elapsedSeconds(turn.Now),
				len(m.state.Findings),
				len(m.state.Fixes),
			),
			StopReason: "complete",
		})
	}

	if m.state.Cycle >= m.config.MaxCycles {
		m.state.Phase = "failed"
		return m.finish(RunModeDirective{
			Status: []string{fmt.Sprintf(
				"Chronos: max cycles (%d) reached without completion",
				m.config.MaxCycles,
			)},
			TerminalText: fmt.Sprintf(
				"\n\n---\nChronos stopped after %d cycles (%.0fs)\nLast findings: %s\n",
				m.config.MaxCycles,
				m.elapsedSeconds(turn.Now),
				strings.Join(m.state.Findings, "; "),
			),
			StopReason: "max_cycles",
		})
	}

	m.state.Cycle++
	m.state.Phase = "find"
	m.state.LastCycleAt = turn.Now
	return RunModeDirective{
		Continue:   true,
		UserPrompt: m.findPrompt(),
		Status:     []string{m.phaseStatus("FIND")},
	}
}

func (m *chronosRunMode) finish(directive RunModeDirective) RunModeDirective {
	directive.Continue = false
	m.finished = true
	m.terminalDirective = cloneRunModeDirective(directive)
	return cloneRunModeDirective(m.terminalDirective)
}

func (m *chronosRunMode) findPrompt() string {
	prompt := fmt.Sprintf(`[Chronos Cycle %d — FIND Phase]
Analyze the current state. What needs to be fixed or changed?
Task: %s`, m.state.Cycle, m.task)
	if m.workDir != "" {
		prompt += "\nWorkspace: " + m.workDir
	}
	if m.state.Cycle > 1 {
		prompt += fmt.Sprintf(
			"\n\nPrevious findings: %s\nPrevious fixes: %s\nVerify result: %s",
			strings.Join(m.state.Findings, "; "),
			strings.Join(m.state.Fixes, "; "),
			strings.Join(m.state.VerifyResults, "; "),
		)
	}
	return prompt + "\n\nReport findings concisely. If everything looks good, respond with [COMPLETE]."
}

func (m *chronosRunMode) fixPrompt() string {
	return fmt.Sprintf(`[Chronos Cycle %d — FIX Phase]
Based on the findings above, make the necessary fixes.
Use tools to edit files, run commands, etc.
Be precise and minimal — only fix what was found.`, m.state.Cycle)
}

func (m *chronosRunMode) verifyPrompt() string {
	if m.config.VerifyCommand != "" {
		return fmt.Sprintf(`[Chronos Cycle %d — VERIFY Phase]
Run the verification command: %s
If it passes, respond with [COMPLETE].
If it fails, analyze the failures for the next FIND phase.`, m.state.Cycle, m.config.VerifyCommand)
	}
	return fmt.Sprintf(`[Chronos Cycle %d — VERIFY Phase]
Verify that the fixes are correct:
1. Read the modified files to confirm changes
2. Run any relevant tests or checks
3. If everything looks good, respond with [COMPLETE]
4. If issues remain, describe them for the next cycle`, m.state.Cycle)
}

func (m *chronosRunMode) phaseStatus(phase string) string {
	return fmt.Sprintf("Chronos cycle %d/%d — %s", m.state.Cycle, m.config.MaxCycles, phase)
}

func (m *chronosRunMode) elapsedSeconds(now time.Time) float64 {
	elapsed := now.Sub(m.state.StartedAt).Seconds()
	if elapsed < 0 {
		return 0
	}
	return elapsed
}

func chronosComplete(text string) bool {
	return strings.Contains(strings.ToUpper(text), chronosCompletionMarker)
}

func cloneRunModeDirective(directive RunModeDirective) RunModeDirective {
	directive.Status = append([]string(nil), directive.Status...)
	return directive
}
