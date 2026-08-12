package agent

import (
	"context"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

// RunChronos is a compatibility adapter over the single agent kernel. Chronos
// owns only FIND/FIX/VERIFY phase decisions; RunLoopWithOptions remains the
// sole owner of providers, tools, context, hooks, evidence, receipts, and the
// terminal event stream.
func RunChronos(
	ctx context.Context,
	provider types.Provider,
	model string,
	task string,
	workDir string,
	cfg ChronosConfig,
	eventCh chan<- Event,
) {
	defaults := DefaultChronosConfig()
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = defaults.MaxIterations
	}
	if cfg.MaxCycles <= 0 {
		cfg.MaxCycles = defaults.MaxCycles
	}
	if cfg.TotalTimeout <= 0 {
		cfg.TotalTimeout = defaults.TotalTimeout
	}

	runCtx, cancel := context.WithTimeout(ctx, cfg.TotalTimeout)
	defer cancel()
	mode := newChronosRunMode(task, workDir, cfg)
	anchor := cfg.PlanAnchor
	if anchor == nil {
		anchor = defaultChronosPlanAnchor(task, cfg)
	}

	RunLoopWithOptions(
		runCtx,
		provider,
		model,
		[]types.Message{{Role: "user", Content: mustJSON(task)}},
		workDir,
		RunOptions{
			SessionID:         cfg.SessionID,
			ApprovalRequester: cfg.ApprovalRequester,
			WorkstreamContext: cfg.WorkstreamContext,
			Recorder:          cfg.Recorder,
			HarnessProfile:    cfg.HarnessProfile,
			CapabilityProfile: cfg.CapabilityProfile,
			PlanAnchor:        anchor,
			SandboxRunner:     cfg.SandboxRunner,
			SandboxPolicy:     cfg.SandboxPolicy,
			PluginDirs:        cloneStringsPreserveNil(cfg.PluginDirs),
			PluginExecution:   clonePluginExecutionOptions(cfg.PluginExecution),
			DisablePlugins:    cfg.DisablePlugins,
			RunMode:           mode,
			IterationLimit:    cfg.MaxIterations,
		},
		eventCh,
	)
}

func defaultChronosPlanAnchor(task string, cfg ChronosConfig) *PlanAnchor {
	objective := strings.TrimSpace(task)
	if objective == "" {
		return nil
	}
	definition := strings.TrimSpace(cfg.CompletionCheck)
	if definition == "" {
		definition = "The Chronos FIND/FIX/VERIFY cycle reaches a verified completion or records the blocking outcome."
	}
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        objective,
		CurrentStep:      "run the bounded FIND/FIX/VERIFY cycle",
		DefinitionOfDone: []string{definition},
	})
	if err != nil {
		return nil
	}
	return &anchor
}

func cloneStringsPreserveNil(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func clonePluginExecutionOptions(options *PluginExecutionOptions) *PluginExecutionOptions {
	if options == nil {
		return nil
	}
	copy := *options
	return &copy
}
