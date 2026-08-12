package agent

import (
	"strings"
	"testing"
	"time"
)

func TestChronosRunModeTransitions(t *testing.T) {
	startedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	type turnExpectation struct {
		text             string
		advance          time.Duration
		wantContinue     bool
		wantPhase        string
		wantCycle        int
		wantPrompt       []string
		wantPromptAbsent []string
		wantStatus       string
		wantStopReason   string
		wantTerminalText string
	}
	tests := []struct {
		name              string
		maxCycles         int
		autoFix           bool
		verifyCommand     string
		turns             []turnExpectation
		wantFindings      []string
		wantFixes         []string
		wantVerifyResults []string
	}{
		{
			name:      "find completes case insensitively",
			maxCycles: 3,
			autoFix:   true,
			turns: []turnExpectation{{
				text:             "Everything is [complete]",
				advance:          3 * time.Second,
				wantPhase:        "complete",
				wantCycle:        1,
				wantStatus:       "Chronos: COMPLETE after 1 cycles",
				wantStopReason:   "complete",
				wantTerminalText: "Chronos completed in 1 cycles (3s)",
			}},
		},
		{
			name:      "find stops when auto fix is disabled",
			maxCycles: 2,
			autoFix:   false,
			turns: []turnExpectation{{
				text:           "one issue remains",
				wantPhase:      "find",
				wantCycle:      1,
				wantStatus:     "Chronos: findings reported (auto-fix disabled)",
				wantStopReason: "auto_fix_disabled",
			}},
			wantFindings: []string{"one issue remains"},
		},
		{
			name:          "find fix verify completes",
			maxCycles:     2,
			autoFix:       true,
			verifyCommand: "go test ./...",
			turns: []turnExpectation{
				{
					text:             "bug in parser",
					wantContinue:     true,
					wantPhase:        "fix",
					wantCycle:        1,
					wantPrompt:       []string{"Cycle 1 — FIX", "make the necessary fixes"},
					wantPromptAbsent: []string{"go test ./..."},
					wantStatus:       "Chronos cycle 1/2 — FIX",
				},
				{
					text:         "parser fixed",
					wantContinue: true,
					wantPhase:    "verify",
					wantCycle:    1,
					wantPrompt:   []string{"Cycle 1 — VERIFY", "go test ./..."},
					wantStatus:   "Chronos cycle 1/2 — VERIFY",
				},
				{
					text:             "tests pass [CoMpLeTe]",
					advance:          5 * time.Second,
					wantPhase:        "complete",
					wantCycle:        1,
					wantStatus:       "Chronos: VERIFIED & COMPLETE after 1 cycles",
					wantStopReason:   "complete",
					wantTerminalText: "Fixes: 1, Verified",
				},
			},
			wantFindings:      []string{"bug in parser"},
			wantFixes:         []string{"parser fixed"},
			wantVerifyResults: []string{"tests pass [CoMpLeTe]"},
		},
		{
			name:      "failed verify starts next cycle",
			maxCycles: 2,
			autoFix:   true,
			turns: []turnExpectation{
				{text: "first finding", wantContinue: true, wantPhase: "fix", wantCycle: 1},
				{text: "first fix", wantContinue: true, wantPhase: "verify", wantCycle: 1},
				{
					text:         "still failing",
					advance:      7 * time.Second,
					wantContinue: true,
					wantPhase:    "find",
					wantCycle:    2,
					wantPrompt: []string{
						"Cycle 2 — FIND",
						"Previous findings: first finding",
						"Previous fixes: first fix",
						"Verify result: still failing",
					},
					wantStatus: "Chronos cycle 2/2 — FIND",
				},
				{
					text:           "nothing remains [COMPLETE]",
					wantPhase:      "complete",
					wantCycle:      2,
					wantStopReason: "complete",
				},
			},
			wantFindings:      []string{"first finding"},
			wantFixes:         []string{"first fix"},
			wantVerifyResults: []string{"still failing"},
		},
		{
			name:      "failed verify exhausts max cycles",
			maxCycles: 1,
			autoFix:   true,
			turns: []turnExpectation{
				{text: "last finding", wantContinue: true, wantPhase: "fix", wantCycle: 1},
				{text: "attempted fix", wantContinue: true, wantPhase: "verify", wantCycle: 1},
				{
					text:             "verification failed",
					advance:          9 * time.Second,
					wantPhase:        "failed",
					wantCycle:        1,
					wantStatus:       "Chronos: max cycles (1) reached without completion",
					wantStopReason:   "max_cycles",
					wantTerminalText: "Chronos stopped after 1 cycles (9s)",
				},
			},
			wantFindings:      []string{"last finding"},
			wantFixes:         []string{"attempted fix"},
			wantVerifyResults: []string{"verification failed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode := newChronosRunMode("repair parser", `D:\workspace`, ChronosConfig{
				MaxCycles:     tt.maxCycles,
				AutoFix:       tt.autoFix,
				VerifyCommand: tt.verifyCommand,
			})
			start, err := mode.Start(startedAt)
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			if !start.Continue || !strings.Contains(start.UserPrompt, "Cycle 1 — FIND") {
				t.Fatalf("Start() directive = %+v", start)
			}
			if mode.CurrentStep() != "find" {
				t.Fatalf("CurrentStep() after start = %q", mode.CurrentStep())
			}

			var directive RunModeDirective
			for i, turn := range tt.turns {
				directive, err = mode.Advance(RunModeTurn{
					Text:      turn.text,
					Iteration: i + 1,
					Now:       startedAt.Add(turn.advance),
				})
				if err != nil {
					t.Fatalf("Advance(%d) error = %v", i, err)
				}
				state := chronosSnapshotState(t, mode.Snapshot(RunModeMetrics{}))
				if directive.Continue != turn.wantContinue {
					t.Errorf("Advance(%d).Continue = %v, want %v", i, directive.Continue, turn.wantContinue)
				}
				if state.Phase != turn.wantPhase || state.Cycle != turn.wantCycle {
					t.Errorf("Advance(%d) state = cycle %d phase %q, want cycle %d phase %q", i, state.Cycle, state.Phase, turn.wantCycle, turn.wantPhase)
				}
				if mode.CurrentStep() != turn.wantPhase {
					t.Errorf("Advance(%d) CurrentStep() = %q, want %q", i, mode.CurrentStep(), turn.wantPhase)
				}
				for _, want := range turn.wantPrompt {
					if !strings.Contains(directive.UserPrompt, want) {
						t.Errorf("Advance(%d) prompt missing %q:\n%s", i, want, directive.UserPrompt)
					}
				}
				for _, unwanted := range turn.wantPromptAbsent {
					if strings.Contains(directive.UserPrompt, unwanted) {
						t.Errorf("Advance(%d) prompt unexpectedly contains %q:\n%s", i, unwanted, directive.UserPrompt)
					}
				}
				if turn.wantStatus != "" && !containsChronosString(directive.Status, turn.wantStatus) {
					t.Errorf("Advance(%d) status = %v, want %q", i, directive.Status, turn.wantStatus)
				}
				if directive.StopReason != turn.wantStopReason {
					t.Errorf("Advance(%d).StopReason = %q, want %q", i, directive.StopReason, turn.wantStopReason)
				}
				if turn.wantTerminalText != "" && !strings.Contains(directive.TerminalText, turn.wantTerminalText) {
					t.Errorf("Advance(%d) terminal text missing %q:\n%s", i, turn.wantTerminalText, directive.TerminalText)
				}
			}

			state := chronosSnapshotState(t, mode.Snapshot(RunModeMetrics{}))
			assertStringsEqual(t, "Findings", state.Findings, tt.wantFindings)
			assertStringsEqual(t, "Fixes", state.Fixes, tt.wantFixes)
			assertStringsEqual(t, "VerifyResults", state.VerifyResults, tt.wantVerifyResults)
		})
	}
}

func TestChronosRunModeVerifyCommandOnlyAppearsInVerifyPrompt(t *testing.T) {
	const command = "go test ./internal/chronos-sentinel"
	mode := newChronosRunMode("repair parser", `D:\workspace`, ChronosConfig{
		MaxCycles:     2,
		AutoFix:       true,
		VerifyCommand: command,
	})
	if suffix := mode.SystemPromptSuffix(); strings.Contains(suffix, command) {
		t.Fatalf("SystemPromptSuffix() leaked verification command:\n%s", suffix)
	}
	startedAt := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	find, err := mode.Start(startedAt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(find.UserPrompt, command) {
		t.Fatalf("FIND prompt leaked verification command:\n%s", find.UserPrompt)
	}
	fix, err := mode.Advance(RunModeTurn{Text: "issue", Now: startedAt})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fix.UserPrompt, command) {
		t.Fatalf("FIX prompt leaked verification command:\n%s", fix.UserPrompt)
	}
	verify, err := mode.Advance(RunModeTurn{Text: "fixed", Now: startedAt})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(verify.UserPrompt, command) {
		t.Fatalf("VERIFY prompt missing verification command:\n%s", verify.UserPrompt)
	}
}

func TestChronosRunModeLifecycleAndTerminalIdempotency(t *testing.T) {
	now := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	mode := newChronosRunMode("task", "", ChronosConfig{MaxCycles: 1, AutoFix: true})
	if _, err := mode.Advance(RunModeTurn{Text: "premature", Now: now}); err == nil {
		t.Fatal("Advance() before Start() error = nil")
	}
	if _, err := mode.Start(now); err != nil {
		t.Fatal(err)
	}
	if _, err := mode.Start(now); err == nil {
		t.Fatal("second Start() error = nil")
	}

	terminal, err := mode.Advance(RunModeTurn{Text: "[COMPLETE]", Now: now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	terminal.Status[0] = "caller mutation"
	repeated, err := mode.Advance(RunModeTurn{Text: "must not change state", Now: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.StopReason != "complete" || repeated.Status[0] == "caller mutation" {
		t.Fatalf("idempotent terminal directive = %+v", repeated)
	}
	state := chronosSnapshotState(t, mode.Snapshot(RunModeMetrics{}))
	if state.Cycle != 1 || state.Phase != "complete" || len(state.Findings) != 0 {
		t.Fatalf("terminal state changed after repeated Advance: %+v", state)
	}
}

func TestChronosRunModeSnapshotCopiesStateAndInjectsKernelMetrics(t *testing.T) {
	now := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	mode := newChronosRunMode("task", `D:\workspace`, ChronosConfig{MaxCycles: 2, AutoFix: true})
	if _, err := mode.Start(now); err != nil {
		t.Fatal(err)
	}
	longFinding := strings.Repeat("x", 250)
	if _, err := mode.Advance(RunModeTurn{Text: longFinding, Now: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}

	metrics := RunModeMetrics{
		TotalTools:      4,
		EstimatedTokens: 1234,
		Sandbox: []SandboxExecutionRecord{{
			ToolID:   "tool-1",
			ToolName: "Bash",
		}},
	}
	snapshot := mode.Snapshot(metrics)
	if snapshot.Name != "chronos" || snapshot.StopReason != "" {
		t.Fatalf("snapshot metadata = %+v", snapshot)
	}
	state := chronosSnapshotState(t, snapshot)
	if state.TotalTools != 4 || state.TotalTokens != 1234 || len(state.Sandbox) != 1 {
		t.Fatalf("snapshot metrics = %+v", state)
	}
	if len(state.Findings) != 1 || len(state.Findings[0]) != 203 || !strings.HasSuffix(state.Findings[0], "...") {
		t.Fatalf("bounded findings = %#v", state.Findings)
	}
	if !state.StartedAt.Equal(now) || !state.LastCycleAt.Equal(now) {
		t.Fatalf("snapshot timestamps = started %v lastCycle %v", state.StartedAt, state.LastCycleAt)
	}

	state.Findings[0] = "mutated"
	state.Sandbox[0].ToolID = "mutated"
	metrics.Sandbox[0].ToolID = "caller-mutated"
	next := chronosSnapshotState(t, mode.Snapshot(RunModeMetrics{
		Sandbox: []SandboxExecutionRecord{{ToolID: "tool-2"}},
	}))
	if next.Findings[0] == "mutated" || next.Sandbox[0].ToolID != "tool-2" {
		t.Fatalf("snapshot aliased caller-owned data: %+v", next)
	}
}

func TestChronosRunModeRejectsInvalidMaxCycles(t *testing.T) {
	mode := newChronosRunMode("task", "", ChronosConfig{})
	if _, err := mode.Start(time.Now()); err == nil || !strings.Contains(err.Error(), "MaxCycles") {
		t.Fatalf("Start() error = %v, want MaxCycles validation", err)
	}
}

func TestChronosRunModeStartDirectiveAndSystemSuffix(t *testing.T) {
	now := time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)
	mode := newChronosRunMode("  repair parser  ", `  D:\workspace  `, ChronosConfig{MaxCycles: 3, AutoFix: true})
	if mode.Name() != "chronos" {
		t.Fatalf("Name() = %q", mode.Name())
	}
	for _, want := range []string{"Chronos Mode: FIND → FIX → VERIFY", "Task: repair parser", "Maximum cycles: 3"} {
		if suffix := mode.SystemPromptSuffix(); !strings.Contains(suffix, want) {
			t.Errorf("SystemPromptSuffix() missing %q:\n%s", want, suffix)
		}
	}
	if mode.CurrentStep() != "" {
		t.Fatalf("CurrentStep() before start = %q", mode.CurrentStep())
	}
	directive, err := mode.Start(now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Cycle 1 — FIND", "Task: repair parser", `Workspace: D:\workspace`, "respond with [COMPLETE]"} {
		if !strings.Contains(directive.UserPrompt, want) {
			t.Errorf("Start() prompt missing %q:\n%s", want, directive.UserPrompt)
		}
	}
	for _, want := range []string{
		"Chronos: starting autonomous loop (max 3 cycles)",
		"Chronos cycle 1/3 — FIND",
	} {
		if !containsChronosString(directive.Status, want) {
			t.Errorf("Start() status = %v, want %q", directive.Status, want)
		}
	}
}

func chronosSnapshotState(t *testing.T, snapshot RunModeSnapshot) ChronosState {
	t.Helper()
	state, ok := snapshot.State.(ChronosState)
	if !ok {
		t.Fatalf("snapshot state type = %T, want ChronosState", snapshot.State)
	}
	return state
}

func containsChronosString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertStringsEqual(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %#v, want %#v", name, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %q, want %q", name, i, got[i], want[i])
		}
	}
}
