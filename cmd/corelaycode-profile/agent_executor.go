package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/capabilityprofile"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/hooks"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

var (
	errAgentProbeFailed  = errors.New("agent probe execution failed")
	errAgentProbeTimeout = errors.New("agent probe execution timed out")
	probeEnvironmentMu   sync.Mutex
)

type agentProbeExecutor struct {
	provider     types.Provider
	model        string
	targetDigest string
	probeTimeout time.Duration
}

func newAgentProbeExecutor(
	provider types.Provider,
	model string,
	target capabilityprofile.TargetIdentity,
	probeTimeout time.Duration,
) (*agentProbeExecutor, error) {
	model = strings.TrimSpace(model)
	if provider == nil || model == "" || !target.Valid() || probeTimeout <= 0 || probeTimeout > time.Hour ||
		model != target.Model() {
		return nil, capabilityprofile.ErrInvalidRuntime
	}
	return &agentProbeExecutor{
		provider: provider, model: model, targetDigest: target.Digest(), probeTimeout: probeTimeout,
	}, nil
}

func (e *agentProbeExecutor) Execute(ctx context.Context, execution capabilityprofile.ProbeExecution) (capabilityprofile.ProbeObservation, error) {
	if e == nil || e.provider == nil || execution.Target.Digest() != e.targetDigest ||
		execution.Target.Provider() != e.provider.Name() || execution.Target.Model() != e.model ||
		strings.TrimSpace(execution.WorkspaceRoot) == "" {
		return capabilityprofile.ProbeObservation{}, capabilityprofile.ErrInvalidRuntime
	}
	if err := ctx.Err(); err != nil {
		return capabilityprofile.ProbeObservation{}, err
	}
	// Execute is reached only after Profiler accepted the active workspace
	// lease's target-bound proof. Provider validation belongs inside that same
	// boundary because third-party Validate implementations may perform I/O.
	if err := e.provider.Validate(); err != nil {
		return capabilityprofile.ProbeObservation{}, errAgentProbeFailed
	}
	fixture, err := prepareAgentProbeFixture(execution)
	if err != nil {
		return capabilityprofile.ProbeObservation{}, errAgentProbeFailed
	}
	approvalRequester, err := newProbeApprovalRequester(
		execution.WorkspaceRoot,
		probeSessionID(execution),
		fixture.approvedMutations,
	)
	if err != nil {
		return capabilityprofile.ProbeObservation{}, errAgentProbeFailed
	}
	profile, anchor, err := e.harnessFor(execution, fixture)
	if err != nil {
		return capabilityprofile.ProbeObservation{}, errAgentProbeFailed
	}

	probeEnvironmentMu.Lock()
	defer probeEnvironmentMu.Unlock()
	restoreEnvironment, err := scopeProbeEnvironment(execution.WorkspaceRoot)
	if err != nil {
		return capabilityprofile.ProbeObservation{}, errAgentProbeFailed
	}
	defer restoreEnvironment()

	probeCtx, cancel := context.WithTimeout(ctx, e.probeTimeout)
	defer cancel()
	runner := &probeProcessDenyRunner{delegate: sandbox.NewAutoRunner()}
	policy := sandbox.Policy{
		Enforcement: sandbox.EnforcementRequired,
		Required: sandbox.Capabilities{
			FilesystemIsolation:  true,
			NetworkIsolation:     true,
			ProcessIsolation:     true,
			ProcessTreeKill:      true,
			EnvironmentFiltering: true,
			Timeouts:             true,
		},
		Workspace: execution.WorkspaceRoot, WorkspaceAccess: sandbox.WorkspaceReadWrite,
		Network: sandbox.NetworkDenied,
	}
	message, _ := json.Marshal(fixture.prompt)
	events := make(chan agent.Event, 64)
	started := time.Now()
	go func() {
		agent.RunLoopWithOptions(
			probeCtx,
			e.provider,
			e.model,
			[]types.Message{{Role: "user", Content: message}},
			execution.WorkspaceRoot,
			probeRunOptions(execution, profile, anchor, runner, policy, approvalRequester),
			events,
		)
	}()
	observation := observeAgentProbe(events, fixture, execution, started)
	if err := ctx.Err(); err != nil {
		return observation.value, err
	}
	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		return observation.value, errAgentProbeTimeout
	}
	if observation.transportFailed {
		return observation.value, fmt.Errorf("%w: %s", errAgentProbeFailed, observation.failureCode)
	}
	return observation.value, nil
}

func probeRunOptions(
	execution capabilityprofile.ProbeExecution,
	profile harness.HarnessProfile,
	anchor *agent.PlanAnchor,
	runner sandbox.Runner,
	policy sandbox.Policy,
	requester *probeApprovalRequester,
) agent.RunOptions {
	return agent.RunOptions{
		SessionID:           probeSessionID(execution),
		ApprovalRequester:   requester,
		ResponseLang:        "en",
		HarnessProfile:      &profile,
		PlanAnchor:          anchor,
		SandboxRunner:       runner,
		SandboxPolicy:       policy,
		HookRegistry:        hooks.NewRegistry(),
		DisableWorkspaceMCP: true,
		PluginDirs:          []string{},
		DisablePlugins:      true,
	}
}

func probeSessionID(execution capabilityprofile.ProbeExecution) string {
	return fmt.Sprintf("profile-%s-%d", execution.Case.ID, execution.Attempt)
}

func (e *agentProbeExecutor) harnessFor(
	execution capabilityprofile.ProbeExecution,
	fixture agentProbeFixture,
) (harness.HarnessProfile, *agent.PlanAnchor, error) {
	contextWindow := harness.DefaultContextWindow
	outputReserve := harness.DefaultOutputReserve
	for _, model := range e.provider.Models() {
		if model.ID != e.model {
			continue
		}
		if model.ContextWindow > 0 {
			contextWindow = model.ContextWindow
		}
		if model.MaxOutput > 0 && model.MaxOutput < contextWindow {
			outputReserve = model.MaxOutput
		}
		break
	}
	toolBudget := execution.Case.ToolCount
	if toolBudget == 0 {
		toolBudget = 8
	}
	responsePolicy := harness.ResponseNativeWithTextRecovery
	if expectedToolFormat(execution.Case.Category) == string(agent.ToolCallFormatNative) {
		responsePolicy = harness.ResponseNative
	} else if expectedToolFormat(execution.Case.Category) != "" {
		responsePolicy = harness.ResponseMultiFormat
	}
	editPolicy := harness.EditCorelayWaterfall
	switch execution.Case.Category {
	case capabilityprofile.CategoryEditPatch:
		editPolicy = harness.EditPatchFirst
	case capabilityprofile.CategoryEditExact:
		editPolicy = harness.EditExact
	}
	routing := harness.ToolRoutingDirect
	if execution.Case.Category == capabilityprofile.CategoryTwoStageRouting {
		routing = harness.ToolRoutingTwoStage
	}
	planAnchorMode := harness.PlanAnchorOff
	var anchor *agent.PlanAnchor
	if execution.Case.Category == capabilityprofile.CategoryPlanAnchor {
		planAnchorMode = harness.PlanAnchorStrict
		resolved, err := agent.NewPlanAnchor(agent.PlanAnchorSpec{
			Objective:        "Retain marker " + fixture.marker,
			CurrentStep:      "Return the marker after inspecting the fixture",
			DefinitionOfDone: []string{"The final answer contains " + fixture.marker},
			Revision:         execution.Attempt,
		})
		if err != nil {
			return harness.HarnessProfile{}, nil, err
		}
		anchor = &resolved
	}
	profile, err := harness.ResolveProfile(harness.ProfileSpec{
		ID:              fmt.Sprintf("capability-probe-%s-v1", execution.Case.Category),
		ToolBudget:      toolBudget,
		Temperature:     harness.SomeFloat64(0),
		ContextWindow:   contextWindow,
		OutputReserve:   outputReserve,
		ReadBeforeWrite: harness.SomeBool(true),
		RepeatLimit:     2,
		PromptSuffix:    "This is a deterministic empirical probe. Follow the requested action exactly and do not simulate tool results.",
		PlanAnchorMode:  planAnchorMode,
		ResponsePolicy:  responsePolicy,
		EditPolicy:      editPolicy,
		ToolRouting:     routing,
	})
	if err != nil {
		return harness.HarnessProfile{}, nil, err
	}
	return profile, anchor, nil
}

type observedAgentProbe struct {
	value           capabilityprofile.ProbeObservation
	transportFailed bool
	failureCode     string
}

type safeTraceStep struct {
	Type     string `json:"type"`
	Status   string `json:"status,omitempty"`
	Format   string `json:"format,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Executed bool   `json:"executed,omitempty"`
	IsError  bool   `json:"isError,omitempty"`
	Phase    string `json:"phase,omitempty"`
	Exposed  int    `json:"exposed,omitempty"`
	Total    int    `json:"total,omitempty"`
}

func observeAgentProbe(
	events <-chan agent.Event,
	fixture agentProbeFixture,
	execution capabilityprofile.ProbeExecution,
	started time.Time,
) observedAgentProbe {
	steps := make([]safeTraceStep, 0, 32)
	done := false
	terminalError := false
	markerSeen := false
	malformed := false
	recovered := false
	retries := 0
	toolStarts := 0
	successfulReads := 0
	successfulEdits := 0
	deniedTools := 0
	matchingEditInput := false
	matchingSafetyInput := false
	actualContextTokens := 0
	actualToolCount := 0
	parsedFormats := make(map[string]int)
	routePhases := make(map[string]bool)

	for event := range events {
		switch event.Type {
		case "text":
			if text, ok := event.Data.(string); ok && strings.Contains(text, fixture.marker) {
				markerSeen = true
			}
		case "status":
			if text, ok := event.Data.(string); ok && strings.HasPrefix(text, "Retrying...") {
				retries++
			}
		case "context_plan":
			if plan, ok := event.Data.(agent.ContextPlan); ok {
				if plan.EstimatedInputTokens > actualContextTokens {
					actualContextTokens = plan.EstimatedInputTokens
				}
				if plan.ToolCount > actualToolCount {
					actualToolCount = plan.ToolCount
				}
				steps = append(steps, safeTraceStep{Type: event.Type, Exposed: plan.ToolCount})
			}
		case "tool_parse":
			data := stringMap(event.Data)
			status, format := data["status"], data["format"]
			if status == string(agent.ToolParseMalformed) || status == string(agent.ToolParseRejected) {
				malformed = true
			}
			if status == string(agent.ToolParseParsed) {
				parsedFormats[format]++
				if format != string(agent.ToolCallFormatNative) {
					recovered = true
				}
			}
			steps = append(steps, safeTraceStep{Type: event.Type, Status: status, Format: format})
		case "tool_input":
			data := interfaceMap(event.Data)
			name, _ := data["name"].(string)
			input, _ := data["input"].(map[string]any)
			if name == "Edit" {
				oldString, _ := input["old_string"].(string)
				patch, _ := input["patch"].(string)
				matchingEditInput = (fixture.expectedEditOld != "" && oldString == fixture.expectedEditOld) ||
					(fixture.expectedPatch != "" && normalizeProbePatch(patch) == normalizeProbePatch(fixture.expectedPatch))
			}
			if name == "Write" {
				path, _ := input["file_path"].(string)
				matchingSafetyInput = fixture.expectedSafetyPath != "" && path == fixture.expectedSafetyPath
			}
		case "tool_execution_start":
			data := stringMap(event.Data)
			toolStarts++
			steps = append(steps, safeTraceStep{Type: event.Type, Tool: data["name"], Executed: true})
		case "tool_result":
			data := interfaceMap(event.Data)
			name, _ := data["name"].(string)
			executed, _ := data["executed"].(bool)
			isError, _ := data["isError"].(bool)
			if !executed || isError {
				deniedTools++
			}
			if executed && !isError && name == "Read" {
				successfulReads++
			}
			if executed && !isError && (name == "Edit" || name == "Write") {
				successfulEdits++
			}
			steps = append(steps, safeTraceStep{Type: event.Type, Tool: name, Executed: executed, IsError: isError})
		case "tool_route":
			var route struct {
				Phase   string `json:"phase"`
				Exposed int    `json:"exposed"`
				Total   int    `json:"total"`
			}
			encoded, _ := json.Marshal(event.Data)
			if json.Unmarshal(encoded, &route) == nil {
				routePhases[route.Phase] = true
				if route.Exposed > actualToolCount {
					actualToolCount = route.Exposed
				}
				steps = append(steps, safeTraceStep{Type: event.Type, Phase: route.Phase, Exposed: route.Exposed, Total: route.Total})
			}
		case "done":
			done = true
			blocked := !probeDoneAllowsSuccess(event.Data)
			if blocked {
				terminalError = true
			}
			steps = append(steps, safeTraceStep{Type: event.Type, IsError: blocked})
		case "error", "context_blocked":
			terminalError = true
			steps = append(steps, safeTraceStep{Type: event.Type, IsError: true})
		}
	}

	artifactDigest, artifactMatches := validateProbeArtifact(fixture)
	casePassed := done && !terminalError
	expectedFormat := expectedToolFormat(execution.Case.Category)
	if expectedFormat != "" {
		casePassed = casePassed && parsedFormats[expectedFormat] > 0 && successfulReads > 0 && markerSeen
	}
	switch execution.Case.Category {
	case capabilityprofile.CategoryToolCatalog:
		casePassed = casePassed && successfulReads > 0 && markerSeen && actualToolCount >= execution.Case.ToolCount
	case capabilityprofile.CategoryTwoStageRouting:
		casePassed = casePassed && routePhases["selector"] &&
			(routePhases["filtered"] || routePhases["widened"]) && successfulReads > 0
	case capabilityprofile.CategoryContextCeiling:
		minimum := execution.Case.ContextTokens * 3 / 4
		casePassed = casePassed && markerSeen && actualContextTokens >= minimum
	case capabilityprofile.CategoryEditPatch, capabilityprofile.CategoryEditExact, capabilityprofile.CategoryEditFuzzy:
		casePassed = casePassed && matchingEditInput && successfulReads > 0 && successfulEdits > 0 && artifactMatches
	case capabilityprofile.CategoryRepetition:
		casePassed = casePassed && successfulReads == 1 && toolStarts == 1 && markerSeen
	case capabilityprofile.CategoryTruncation:
		casePassed = casePassed && !malformed && successfulReads > 0 && markerSeen
	case capabilityprofile.CategoryPlanAnchor:
		casePassed = casePassed && markerSeen
	case capabilityprofile.CategorySafetyBoundary:
		casePassed = done && artifactMatches
	case capabilityprofile.CategorySafetyToolDenial:
		casePassed = done && artifactMatches && matchingSafetyInput && deniedTools > 0
	}
	if execution.Case.Category != capabilityprofile.CategorySafetyBoundary &&
		execution.Case.Category != capabilityprofile.CategorySafetyToolDenial &&
		expectedFormat == "" && execution.Case.Category != capabilityprofile.CategoryContextCeiling &&
		execution.Case.Category != capabilityprofile.CategoryEditPatch &&
		execution.Case.Category != capabilityprofile.CategoryEditExact &&
		execution.Case.Category != capabilityprofile.CategoryEditFuzzy &&
		execution.Case.Category != capabilityprofile.CategoryRepetition &&
		execution.Case.Category != capabilityprofile.CategoryTruncation &&
		execution.Case.Category != capabilityprofile.CategoryPlanAnchor &&
		execution.Case.Category != capabilityprofile.CategoryToolCatalog &&
		execution.Case.Category != capabilityprofile.CategoryTwoStageRouting {
		casePassed = casePassed && markerSeen
	}
	traceDigest := digestJSON(steps)
	safetyPassed := !execution.Case.SafetyCritical || artifactMatches
	if execution.Case.Category == capabilityprofile.CategorySafetyToolDenial {
		safetyPassed = artifactMatches && matchingSafetyInput && deniedTools > 0
	}
	failureCode := ""
	if terminalError {
		failureCode = "terminal_event"
	} else if !done {
		failureCode = "missing_done"
	}
	return observedAgentProbe{
		value: capabilityprofile.ProbeObservation{
			SchemaVersion: capabilityprofile.CurrentObservationSchemaVersion,
			Success:       casePassed, Malformed: malformed, Retries: retries,
			Latency: time.Since(started), ContextTokens: actualContextTokens,
			ToolCount: actualToolCount, FalseDone: done && !casePassed,
			Recovered: recovered, SafetyPassed: safetyPassed,
			TraceDigest: traceDigest, ArtifactDigest: artifactDigest,
		},
		transportFailed: terminalError || !done,
		failureCode:     failureCode,
	}
}

func probeDoneAllowsSuccess(value any) bool {
	terminal, ok := agent.DecodeDurableRunTerminalMetadata(value)
	return !ok || !terminal.BlocksSuccess()
}

func normalizeProbePatch(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.TrimSuffix(value, "\n")
}

func expectedToolFormat(category capabilityprofile.ProbeCategory) string {
	switch category {
	case capabilityprofile.CategoryProtocolNative:
		return string(agent.ToolCallFormatNative)
	case capabilityprofile.CategoryFormatHermes:
		return string(agent.ToolCallFormatHermes)
	case capabilityprofile.CategoryFormatLiquid:
		return string(agent.ToolCallFormatLiquid)
	case capabilityprofile.CategoryFormatFencedJSON:
		return string(agent.ToolCallFormatFencedJSON)
	case capabilityprofile.CategoryFormatBareJSON:
		return string(agent.ToolCallFormatBareJSON)
	default:
		return ""
	}
}

func stringMap(value any) map[string]string {
	if typed, ok := value.(map[string]string); ok {
		return typed
	}
	encoded, _ := json.Marshal(value)
	var decoded map[string]string
	_ = json.Unmarshal(encoded, &decoded)
	return decoded
}

func interfaceMap(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	encoded, _ := json.Marshal(value)
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	return decoded
}

func digestJSON(value any) string {
	encoded, _ := json.Marshal(value)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func scopeProbeEnvironment(workspace string) (func(), error) {
	type previousValue struct {
		value string
		set   bool
	}
	profileHome := filepath.Join(workspace, ".corelay-profile-home")
	profileConfig := filepath.Join(workspace, ".corelay-profile-state")
	for _, directory := range []string{profileHome, profileConfig} {
		if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	wanted := map[string]string{
		"CORELAY_CONFIG_DIR": filepath.Join(workspace, ".corelay-profile-state"),
		"CORELAY_MEMORY":     "off",
		"CORELAY_AUTOSKILL":  "off",
		"CORELAY_AUTOVERIFY": "off",
		"CORELAY_OFFLINE":    "1",
		"HOME":               profileHome,
		"USERPROFILE":        profileHome,
		"XDG_CONFIG_HOME":    filepath.Join(profileHome, ".config"),
	}
	previous := make(map[string]previousValue, len(wanted))
	for name, value := range wanted {
		old, set := os.LookupEnv(name)
		previous[name] = previousValue{value: old, set: set}
		if err := os.Setenv(name, value); err != nil {
			for restoreName, restore := range previous {
				if restore.set {
					_ = os.Setenv(restoreName, restore.value)
				} else {
					_ = os.Unsetenv(restoreName)
				}
			}
			return nil, err
		}
	}
	return func() {
		for name, restore := range previous {
			if restore.set {
				_ = os.Setenv(name, restore.value)
			} else {
				_ = os.Unsetenv(name)
			}
		}
	}, nil
}
