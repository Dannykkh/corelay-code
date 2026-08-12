package agent

import "time"

// RunMode controls optional, run-local orchestration without owning execution.
// Provider calls, tools, context planning, evidence, hooks, and terminal events
// remain the responsibility of RunLoopWithOptions.
type RunMode interface {
	Name() string
	SystemPromptSuffix() string
	Start(time.Time) (RunModeDirective, error)
	Advance(RunModeTurn) (RunModeDirective, error)
	CurrentStep() string
	Snapshot(RunModeMetrics) RunModeSnapshot
}

// RunModeTurn is the completed assistant turn observed by a run mode.
type RunModeTurn struct {
	Text      string
	Iteration int
	Now       time.Time
}

// RunModeDirective tells the common run loop whether another mode step is
// required. It never executes the step itself.
type RunModeDirective struct {
	Continue     bool
	UserPrompt   string
	Status       []string
	TerminalText string
	StopReason   string
}

// RunModeMetrics are kernel-owned observations supplied when a mode snapshot
// is produced.
type RunModeMetrics struct {
	TotalTools      int
	EstimatedTokens int
	Sandbox         []SandboxExecutionRecord
}

// RunModeSnapshot is mode-specific state attached to common run metadata.
type RunModeSnapshot struct {
	Name       string `json:"name"`
	StopReason string `json:"stopReason,omitempty"`
	State      any    `json:"state,omitempty"`
}
