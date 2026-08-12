package agent

import (
	"time"

	"github.com/Dannykkh/corelay-code/internal/approval"
	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

// ChronosConfig controls the bounded FIND/FIX/VERIFY run mode. Provider,
// context, tool, hook, evidence, receipt, and terminal behavior are owned by
// RunLoopWithOptions through the RunChronos adapter.
type ChronosConfig struct {
	MaxIterations     int                                   // overall iteration cap (default 50)
	MaxCycles         int                                   // FIND→FIX→VERIFY cycle cap (default 10)
	CycleTimeout      time.Duration                         // retained compatibility setting
	TotalTimeout      time.Duration                         // max total time (default 30 min)
	VerifyCommand     string                                // command to verify (e.g., "npm test", "go test ./...")
	CompletionCheck   string                                // how to determine completion
	AutoFix           bool                                  // automatically attempt fixes on verify failure
	WorkstreamContext string                                // durable workstream context rendered by the server
	SessionID         string                                `json:"-"`
	ApprovalRequester approval.Requester                    `json:"-"`
	SandboxRunner     sandbox.Runner                        `json:"-"`
	SandboxPolicy     sandbox.Policy                        `json:"-"`
	PluginDirs        []string                              `json:"-"`
	PluginExecution   *PluginExecutionOptions               `json:"-"`
	DisablePlugins    bool                                  `json:"-"`
	Recorder          RunRecorder                           `json:"-"`
	HarnessProfile    *harness.HarnessProfile               `json:"-"`
	CapabilityProfile *capabilityprofile.AutomaticSelection `json:"-"`
	PlanAnchor        *PlanAnchor                           `json:"-"`
}

// DefaultChronosConfig returns standard settings.
func DefaultChronosConfig() ChronosConfig {
	return ChronosConfig{
		MaxIterations: 50,
		MaxCycles:     10,
		CycleTimeout:  5 * time.Minute,
		TotalTimeout:  30 * time.Minute,
		AutoFix:       true,
	}
}

// ChronosState is the transport-safe snapshot emitted by the Chronos RunMode.
type ChronosState struct {
	Cycle         int                      `json:"cycle"`
	Phase         string                   `json:"phase"` // find, fix, verify, complete, failed
	TotalTools    int                      `json:"totalTools"`
	TotalTokens   int                      `json:"totalTokens"`
	Findings      []string                 `json:"findings"`
	Fixes         []string                 `json:"fixes"`
	VerifyResults []string                 `json:"verifyResults"`
	StartedAt     time.Time                `json:"startedAt"`
	LastCycleAt   time.Time                `json:"lastCycleAt"`
	Sandbox       []SandboxExecutionRecord `json:"sandbox,omitempty"`
}
