package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/harness"
)

func TestNewPlanAnchorCopiesAndNormalizesState(t *testing.T) {
	remaining := []string{" inspect repository ", "implement change", "inspect repository"}
	done := []string{"focused tests pass", " focused tests pass ", "diff is scoped"}
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "  absorb\ncapabilities safely ",
		CurrentStep:      " inspect   repository ",
		RemainingSteps:   remaining,
		DefinitionOfDone: done,
		Revision:         2,
	})
	if err != nil {
		t.Fatalf("NewPlanAnchor() error = %v", err)
	}
	if !anchor.Valid() {
		t.Fatal("NewPlanAnchor() returned an invalid anchor")
	}
	if got, want := anchor.Objective(), "absorb capabilities safely"; got != want {
		t.Fatalf("Objective() = %q, want %q", got, want)
	}
	if got, want := anchor.CurrentStep(), "inspect repository"; got != want {
		t.Fatalf("CurrentStep() = %q, want %q", got, want)
	}
	if got, want := anchor.RemainingSteps(), []string{"inspect repository", "implement change"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RemainingSteps() = %#v, want %#v", got, want)
	}
	if got, want := anchor.DefinitionOfDone(), []string{"focused tests pass", "diff is scoped"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DefinitionOfDone() = %#v, want %#v", got, want)
	}

	remaining[0] = "mutated input"
	done[0] = "mutated input"
	returned := anchor.RemainingSteps()
	returned[0] = "mutated output"
	if got, want := anchor.RemainingSteps()[0], "inspect repository"; got != want {
		t.Fatalf("caller mutation changed remaining step to %q", got)
	}
	if got, want := anchor.DefinitionOfDone()[0], "focused tests pass"; got != want {
		t.Fatalf("caller mutation changed Definition of Done to %q", got)
	}
}

func TestPlanAnchorWithProgressCreatesNewRevision(t *testing.T) {
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "ship a verified change",
		CurrentStep:      "inspect",
		RemainingSteps:   []string{"implement", "test"},
		DefinitionOfDone: []string{"tests pass"},
		Revision:         4,
	})
	if err != nil {
		t.Fatalf("NewPlanAnchor() error = %v", err)
	}

	nextSteps := []string{"report"}
	next, err := anchor.WithProgress("test", nextSteps)
	if err != nil {
		t.Fatalf("WithProgress() error = %v", err)
	}
	nextSteps[0] = "mutated"

	if got, want := next.Revision(), 5; got != want {
		t.Fatalf("next Revision() = %d, want %d", got, want)
	}
	if got, want := next.CurrentStep(), "test"; got != want {
		t.Fatalf("next CurrentStep() = %q, want %q", got, want)
	}
	if got, want := next.RemainingSteps(), []string{"report"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("next RemainingSteps() = %#v, want %#v", got, want)
	}
	if got, want := anchor.Revision(), 4; got != want {
		t.Fatalf("original Revision() = %d, want %d", got, want)
	}
	if got, want := anchor.CurrentStep(), "inspect"; got != want {
		t.Fatalf("original CurrentStep() = %q, want %q", got, want)
	}
}

func TestPlanAnchorRenderModesAndEscaping(t *testing.T) {
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "edit <config> & verify",
		CurrentStep:      "apply patch",
		RemainingSteps:   []string{"run tests"},
		DefinitionOfDone: []string{"tests pass"},
		Revision:         1,
	})
	if err != nil {
		t.Fatalf("NewPlanAnchor() error = %v", err)
	}

	off, err := anchor.Render(harness.PlanAnchorOff)
	if err != nil {
		t.Fatalf("Render(off) error = %v", err)
	}
	if off != "" {
		t.Fatalf("Render(off) = %q, want empty", off)
	}

	compact, err := anchor.Render(harness.PlanAnchorCompact)
	if err != nil {
		t.Fatalf("Render(compact) error = %v", err)
	}
	for _, fragment := range []string{
		"<plan-anchor revision=\"1\">",
		"Objective: edit &lt;config&gt; &amp; verify",
		"Current step: apply patch",
		"Remaining steps:",
		"- run tests",
		"Definition of Done:",
		"- tests pass",
		"</plan-anchor>",
	} {
		if !strings.Contains(compact, fragment) {
			t.Fatalf("Render(compact) missing %q", fragment)
		}
	}
	if strings.Contains(compact, "Completion rule:") {
		t.Fatal("Render(compact) unexpectedly contains strict completion rule")
	}

	strict, err := anchor.Render(harness.PlanAnchorStrict)
	if err != nil {
		t.Fatalf("Render(strict) error = %v", err)
	}
	if !strings.Contains(strict, "Completion rule:") {
		t.Fatal("Render(strict) is missing completion rule")
	}
	if _, err := anchor.Render(harness.PlanAnchorMode("unknown")); err == nil {
		t.Fatal("Render() accepted an invalid mode")
	}
	if _, err := (PlanAnchor{}).Render(harness.PlanAnchorCompact); err == nil {
		t.Fatal("Render() accepted an unresolved anchor")
	}
}

func TestNewPlanAnchorRejectsInvalidState(t *testing.T) {
	tests := []struct {
		name string
		spec PlanAnchorSpec
	}{
		{
			name: "missing objective",
			spec: PlanAnchorSpec{DefinitionOfDone: []string{"tests pass"}},
		},
		{
			name: "missing Definition of Done",
			spec: PlanAnchorSpec{Objective: "implement"},
		},
		{
			name: "negative revision",
			spec: PlanAnchorSpec{
				Objective:        "implement",
				DefinitionOfDone: []string{"tests pass"},
				Revision:         -1,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewPlanAnchor(tt.spec); err == nil {
				t.Fatal("NewPlanAnchor() error = nil, want validation error")
			}
		})
	}
	if _, err := (PlanAnchor{}).WithProgress("next", nil); err == nil {
		t.Fatal("WithProgress() accepted an unresolved anchor")
	}
}

func TestLegacyModelProfileAdapterPreservesPrecedenceAndDefaults(t *testing.T) {
	tests := []struct {
		model       string
		wantMatched bool
		wantName    string
		wantID      string
		wantTools   int
	}{
		{"QWEN-CODER:3B", true, "coder (coding-tuned)", "local-coder", 16},
		{"devstral:latest", true, "devstral (agentic-coding)", "local-devstral", 16},
		{"deepseek-r1:8b", true, "deepseek-r1 (reasoning)", "local-deepseek-r1", 14},
		{"qwq:latest", true, "qwq (reasoning)", "local-qwq", 14},
		{"qwen3:3b", true, "small (<=4B)", "local-small-3b", 8},
		{"QWEN3:4B", true, "small (<=4B)", "local-small-4b", 8},
		{"qwen3:7b", true, "small (7-8B)", "local-small-7b", 10},
		{"qwen3:8b", true, "small (7-8B)", "local-small-8b", 10},
		{"llama3:70b", false, "local-default", "local-default", defaultLocalToolBudget},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			profile, matched := profileFor(tt.model)
			if matched != tt.wantMatched {
				t.Fatalf("profileFor() matched = %v, want %v", matched, tt.wantMatched)
			}
			if profile.name != tt.wantName {
				t.Fatalf("profileFor() name = %q, want %q", profile.name, tt.wantName)
			}
			if profile.toolBudget != tt.wantTools {
				t.Fatalf("profileFor() tool budget = %d, want %d", profile.toolBudget, tt.wantTools)
			}
			if profile.temperature != defaultLocalTemperature {
				t.Fatalf(
					"profileFor() temperature = %v, want %v",
					profile.temperature,
					defaultLocalTemperature,
				)
			}

			resolved := profile.HarnessProfile()
			if !resolved.Valid() {
				t.Fatal("legacy adapter returned an invalid HarnessProfile")
			}
			if got := resolved.ID(); got != tt.wantID {
				t.Fatalf("HarnessProfile ID() = %q, want %q", got, tt.wantID)
			}
			if got := resolved.ToolBudget(); got != tt.wantTools {
				t.Fatalf("HarnessProfile ToolBudget() = %d, want %d", got, tt.wantTools)
			}
			if got := resolved.ContextWindow(); got != harness.DefaultContextWindow {
				t.Fatalf("HarnessProfile ContextWindow() = %d, want %d", got, harness.DefaultContextWindow)
			}
			if got := resolved.OutputReserve(); got != harness.DefaultOutputReserve {
				t.Fatalf("HarnessProfile OutputReserve() = %d, want %d", got, harness.DefaultOutputReserve)
			}
			if !resolved.ReadBeforeWrite() {
				t.Fatal("HarnessProfile ReadBeforeWrite() = false, want true")
			}
			if got := resolved.RepeatLimit(); got != harness.DefaultRepeatLimit {
				t.Fatalf("HarnessProfile RepeatLimit() = %d, want %d", got, harness.DefaultRepeatLimit)
			}
			if got := resolved.PlanAnchorMode(); got != harness.PlanAnchorOff {
				t.Fatalf("HarnessProfile PlanAnchorMode() = %q, want %q", got, harness.PlanAnchorOff)
			}
			if temperature, ok := resolved.Temperature(); !ok || temperature != defaultLocalTemperature {
				t.Fatalf(
					"HarnessProfile Temperature() = (%v, %v), want (%v, true)",
					temperature,
					ok,
					defaultLocalTemperature,
				)
			}
		})
	}
}
