package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/harness"
	"github.com/Dannykkh/corelay-code/internal/translate"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestRunLoopReadOnlyWeightedBudgetSendsCollapsedPayload(t *testing.T) {
	isolateEvidenceLoopTest(t)
	t.Setenv("CORELAY_OFFLINE", "1")

	if _, err := config.Update(func(cfg *config.Config) error {
		cfg.ReadOnlyExploreRounds = 2
		return nil
	}); err != nil {
		t.Fatalf("configure read-only exploration budget: %v", err)
	}

	workDir := t.TempDir()
	const readSentinel = "WEIGHTED-BUDGET-READ-SENTINEL"
	if err := os.WriteFile(filepath.Join(workDir, "proof.txt"), []byte(readSentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var collapsedRequest string
	provider := &completionLoopProvider{steps: []completionLoopStep{
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("nav-before-read", "LS", map[string]string{"path": "."}), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("content-read", "Read", map[string]string{"file_path": "proof.txt"}), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("nav-at-boundary", "LS", map[string]string{"path": "."}), nil
		},
		func(request *types.MessagesRequest) (scriptedLoopStep, error) {
			collapsedRequest = lastUserText(request.Messages)
			return textStep("The file contains the weighted-budget sentinel."), nil
		},
	}}
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:              "read-only-weighted-payload",
		ToolBudget:      32,
		ContextWindow:   65_536,
		OutputReserve:   4_096,
		MaxIterations:   12,
		PlanAnchorMode:  harness.PlanAnchorOff,
		ToolRouting:     harness.ToolRoutingDirect,
		ReadBeforeWrite: harness.SomeBool(false),
	})
	messages := []types.Message{
		{Role: "user", Content: mustJSON("Keep the API authorization boundary unchanged in all answers.")},
		{Role: "assistant", Content: mustJSON("Understood.")},
		{Role: "user", Content: mustJSON("Where is the proof file in this workspace?")},
	}
	events := make(chan Event, 64)
	go RunLoopWithOptions(context.Background(), provider, "fake-model", messages, workDir, RunOptions{
		HarnessProfile:      &profile,
		DisablePlugins:      true,
		DisableWorkspaceMCP: true,
		EvidencePolicy:      EvidencePolicyConfig{Policy: EvidencePolicyOff},
	}, events)
	for range events {
	}

	requests, errs := provider.snapshot()
	if len(errs) != 0 {
		t.Fatalf("fake provider callbacks failed: %v", errs)
	}
	if len(requests) != 4 {
		t.Fatalf("provider requests = %d, want three exploration calls plus one collapsed-answer call", len(requests))
	}
	if len(requests[2].Tools) == 0 || !requestHasTool(requests[2], "LS") || !requestHasTool(requests[2], "Read") {
		t.Fatalf("request at weighted threshold should still be the final exploration payload, tools=%v", toolDefNames(requests[2].Tools))
	}
	finalRequest := requests[3]
	if len(finalRequest.Messages) != 1 || finalRequest.Messages[0].Role != "user" {
		t.Fatalf("collapsed request messages = %+v, want one user context message", finalRequest.Messages)
	}
	if collapsedRequest == "" {
		collapsedRequest = lastUserText(finalRequest.Messages)
	}
	for _, want := range []string{
		"Where is the proof file in this workspace?",
		"## Context I gathered from the codebase",
		"### LS",
		"### Read",
		readSentinel,
		"The initial weighted exploration budget is reached.",
		"You may use up to 2 additional weighted read-only rounds",
	} {
		if !strings.Contains(collapsedRequest, want) {
			t.Fatalf("collapsed provider prompt is missing %q:\n%s", want, collapsedRequest)
		}
	}
	if !strings.Contains(collapsedRequest, "Keep the API authorization boundary unchanged") {
		t.Fatalf("read-only checkpoint dropped an earlier user constraint:\n%s", collapsedRequest)
	}
	if !requestHasTool(finalRequest, "Read") || !requestHasTool(finalRequest, "LS") {
		t.Fatalf("budget checkpoint should retain targeted read-only tools: %v", toolDefNames(finalRequest.Tools))
	}
	for _, tool := range finalRequest.Tools {
		if !planModeTools[tool.Name] {
			t.Fatalf("budget checkpoint advertised non-read-only tool %q", tool.Name)
		}
	}
}

func TestRunLoopReadOnlyContinuationIsBoundedAndReportsPartialStop(t *testing.T) {
	isolateEvidenceLoopTest(t)
	t.Setenv("CORELAY_OFFLINE", "1")
	if _, err := config.Update(func(cfg *config.Config) error {
		cfg.ReadOnlyExploreRounds = defaultReadOnlyExploreRounds
		return nil
	}); err != nil {
		t.Fatalf("configure initial read-only exploration budget: %v", err)
	}

	workDir := t.TempDir()
	for index := 0; index < 8; index++ {
		dir := filepath.Join(workDir, fmt.Sprintf("nav-%d", index))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "entry.txt"), []byte("navigation fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const initialSentinel = "INITIAL-WEIGHTED-EVIDENCE"
	const firstFollowupSentinel = "FOLLOWUP-EVIDENCE-ONE"
	const secondFollowupSentinel = "FOLLOWUP-EVIDENCE-TWO"
	for path, content := range map[string]string{
		"initial.txt":    initialSentinel,
		"followup-a.txt": firstFollowupSentinel,
		"followup-b.txt": secondFollowupSentinel,
	} {
		if err := os.WriteFile(filepath.Join(workDir, path), []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	steps := make([]completionLoopStep, 0, 12)
	for index := 0; index < 8; index++ {
		path := fmt.Sprintf("nav-%d", index)
		steps = append(steps, func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("navigate-"+path, "LS", map[string]string{"path": path}), nil
		})
	}
	steps = append(steps, func(*types.MessagesRequest) (scriptedLoopStep, error) {
		return toolUseStep("read-initial", "Read", map[string]string{"file_path": "initial.txt"}), nil
	})
	steps = append(steps, func(request *types.MessagesRequest) (scriptedLoopStep, error) {
		prompt := lastUserText(request.Messages)
		if !strings.Contains(prompt, "up to 2 additional weighted read-only rounds") || !requestHasTool(request, "Read") {
			return scriptedLoopStep{}, fmt.Errorf("initial budget checkpoint did not enable bounded targeted reads")
		}
		for _, tool := range request.Tools {
			if !planModeTools[tool.Name] {
				return scriptedLoopStep{}, fmt.Errorf("continuation advertised non-read-only tool %q", tool.Name)
			}
		}
		return toolUseStep("read-followup-a", "Read", map[string]string{"file_path": "followup-a.txt"}), nil
	})
	steps = append(steps, func(request *types.MessagesRequest) (scriptedLoopStep, error) {
		if len(request.Messages) == 0 || !strings.Contains(string(request.Messages[len(request.Messages)-1].Content), firstFollowupSentinel) {
			return scriptedLoopStep{}, fmt.Errorf("first bounded follow-up evidence was not carried into the next provider request")
		}
		return toolUseStep("read-followup-b", "Read", map[string]string{"file_path": "followup-b.txt"}), nil
	})
	steps = append(steps, func(request *types.MessagesRequest) (scriptedLoopStep, error) {
		if len(request.Tools) != 0 {
			return scriptedLoopStep{}, fmt.Errorf("hard read-only boundary still advertises tools: %v", toolDefNames(request.Tools))
		}
		return textStep("Partial answer based on the available evidence."), nil
	})
	provider := &completionLoopProvider{steps: steps}
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:              "read-only-bounded-continuation",
		ToolBudget:      32,
		ContextWindow:   65_536,
		OutputReserve:   4_096,
		MaxIterations:   20,
		PlanAnchorMode:  harness.PlanAnchorOff,
		ToolRouting:     harness.ToolRoutingDirect,
		ReadBeforeWrite: harness.SomeBool(false),
	})
	events := make(chan Event, 128)
	go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{{
		Role: "user", Content: mustJSON("How does the initial workflow connect to its follow-up evidence?"),
	}}, workDir, RunOptions{
		HarnessProfile:      &profile,
		DisablePlugins:      true,
		DisableWorkspaceMCP: true,
		EvidencePolicy:      EvidencePolicyConfig{Policy: EvidencePolicyOff},
	}, events)
	var collected []Event
	for event := range events {
		collected = append(collected, event)
	}

	requests, errs := provider.snapshot()
	if len(errs) != 0 {
		t.Fatalf("fake provider callbacks failed: %v", errs)
	}
	if len(requests) != 12 {
		t.Fatalf("provider requests = %d, want 9 initial + 2 bounded follow-up + 1 partial-answer request", len(requests))
	}
	finalPrompt := lastUserText(requests[len(requests)-1].Messages)
	for _, want := range []string{
		"How does the initial workflow connect to its follow-up evidence?",
		initialSentinel,
		firstFollowupSentinel,
		secondFollowupSentinel,
		"bounded read-only continuation budget is exhausted",
		"best partial answer",
		"name what remains uncertain or unverified",
	} {
		if !strings.Contains(finalPrompt, want) {
			t.Fatalf("hard-boundary prompt is missing %q:\n%s", want, finalPrompt)
		}
	}
	var stopStatus string
	for _, event := range collected {
		if event.Type == "status" && strings.Contains(fmt.Sprint(event.Data), "Read-only exploration stopped:") {
			stopStatus = fmt.Sprint(event.Data)
		}
	}
	if !strings.Contains(stopStatus, "partial") || !strings.Contains(stopStatus, "exhausted") {
		t.Fatalf("explicit stop reason status = %q", stopStatus)
	}
}

func TestRunLoopReadOnlyCheckpointKeepsRoutedCatalogAndDispatchReadOnly(t *testing.T) {
	isolateEvidenceLoopTest(t)
	t.Setenv("CORELAY_OFFLINE", "1")
	if _, err := config.Update(func(cfg *config.Config) error {
		cfg.ReadOnlyExploreRounds = 1
		return nil
	}); err != nil {
		t.Fatalf("configure read-only exploration budget: %v", err)
	}

	workDir := t.TempDir()
	for path, content := range map[string]string{
		"initial.txt":    "initial read evidence\n",
		"followup.txt":   "follow-up read evidence\n",
		"unexpected.txt": "must not be created\n",
	} {
		if path == "unexpected.txt" {
			continue
		}
		if err := os.WriteFile(filepath.Join(workDir, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	provider := &completionLoopProvider{steps: []completionLoopStep{
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("select-read", translate.ToolCategorySelectorName(), map[string]string{
				"category": string(translate.ToolCategoryRead),
			}), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("initial-read", "Read", map[string]string{"file_path": "initial.txt"}), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return toolUseStep("followup-read", "Read", map[string]string{"file_path": "followup.txt"}), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			// Simulate a model violating the read-only continuation contract. The
			// same request catalog also supplies the immutable dispatch allow-list.
			return toolUseStep("unexpected-write", "Write", map[string]string{
				"file_path": "unexpected.txt", "content": "must not be created\n",
			}), nil
		},
		func(*types.MessagesRequest) (scriptedLoopStep, error) {
			return textStep("Partial answer based on read-only evidence."), nil
		},
	}}
	profile := harness.MustResolveProfile(harness.ProfileSpec{
		ID:              "read-only-routed-continuation",
		ToolBudget:      64,
		ContextWindow:   65_536,
		OutputReserve:   4_096,
		MaxIterations:   8,
		PlanAnchorMode:  harness.PlanAnchorOff,
		ToolRouting:     harness.ToolRoutingTwoStage,
		ReadBeforeWrite: harness.SomeBool(false),
	})
	events := make(chan Event, 128)
	go RunLoopWithOptions(context.Background(), provider, "fake-model", []types.Message{{
		Role: "user", Content: mustJSON("Where is the initial proof file?"),
	}}, workDir, RunOptions{
		HarnessProfile:      &profile,
		DisablePlugins:      true,
		DisableWorkspaceMCP: true,
		EvidencePolicy:      EvidencePolicyConfig{Policy: EvidencePolicyOff},
	}, events)
	for range events {
	}

	requests, errs := provider.snapshot()
	if len(errs) != 0 {
		t.Fatalf("fake provider callbacks failed: %v", errs)
	}
	if len(requests) != 5 {
		t.Fatalf("provider requests = %d, want selector + initial read + two bounded turns + final answer", len(requests))
	}
	for index, request := range requests[2:4] {
		if !requestHasTool(request, "Read") {
			t.Fatalf("routed read-only request %d lacks Read: %v", index+2, toolDefNames(request.Tools))
		}
		for _, tool := range request.Tools {
			if !planModeTools[tool.Name] {
				t.Fatalf("routed continuation request %d exposed non-read-only tool %q: %v", index+2, tool.Name, toolDefNames(request.Tools))
			}
		}
	}
	for _, tool := range requests[4].Tools {
		t.Fatalf("hard read-only boundary advertised %q", tool.Name)
	}
	if _, err := os.Stat(filepath.Join(workDir, "unexpected.txt")); !os.IsNotExist(err) {
		t.Fatalf("out-of-catalog Write was not blocked by the dispatch allow-list (stat err=%v)", err)
	}
}
