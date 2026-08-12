package agent

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestCanonicalActionFingerprintNormalizesJSON(t *testing.T) {
	left := ActionFingerprintInput{
		ToolName:       "Edit",
		Arguments:      json.RawMessage(`{"file_path":"a.go","old":"x","new":"y"}`),
		TargetRevision: "sha256:before",
		FailureCode:    "exact_match_failed",
		PlanStep:       "update parser",
	}
	right := left
	right.Arguments = json.RawMessage(" { \"new\" : \"y\", \"old\":\"x\", \"file_path\" : \"a.go\" } \n")

	if got, want := CanonicalActionFingerprint(left), CanonicalActionFingerprint(right); got != want {
		t.Fatalf("equivalent JSON fingerprints differ: %s != %s", got, want)
	}

	empty := left
	empty.Arguments = nil
	emptyObject := left
	emptyObject.Arguments = json.RawMessage(`{}`)
	if got, want := CanonicalActionFingerprint(empty), CanonicalActionFingerprint(emptyObject); got != want {
		t.Fatalf("empty arguments fingerprints differ: %s != %s", got, want)
	}
}

func TestCanonicalActionFingerprintIncludesProgressState(t *testing.T) {
	base := ActionFingerprintInput{
		ToolName:       "Edit",
		Arguments:      json.RawMessage(`{"file_path":"a.go"}`),
		TargetRevision: "r1",
		FailureCode:    "failed",
		PlanStep:       "step-1",
	}
	baseFingerprint := CanonicalActionFingerprint(base)

	tests := []struct {
		name   string
		mutate func(*ActionFingerprintInput)
	}{
		{name: "tool", mutate: func(input *ActionFingerprintInput) { input.ToolName = "Write" }},
		{name: "arguments", mutate: func(input *ActionFingerprintInput) { input.Arguments = json.RawMessage(`{"file_path":"b.go"}`) }},
		{name: "target revision", mutate: func(input *ActionFingerprintInput) { input.TargetRevision = "r2" }},
		{name: "failure code", mutate: func(input *ActionFingerprintInput) { input.FailureCode = "different" }},
		{name: "plan step", mutate: func(input *ActionFingerprintInput) { input.PlanStep = "step-2" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			test.mutate(&changed)
			if got := CanonicalActionFingerprint(changed); got == baseFingerprint {
				t.Fatalf("changed input retained fingerprint %s", got)
			}
		})
	}
}

func TestRunGuardStopsRepeatedNoProgressAction(t *testing.T) {
	guard := NewRunGuard(2)
	action := ActionFingerprintInput{
		ToolName:       "Edit",
		Arguments:      json.RawMessage(`{"file_path":"a.go"}`),
		TargetRevision: "r1",
		FailureCode:    "edit_failed",
		PlanStep:       "step-1",
	}

	first := guard.Observe(action)
	second := guard.Observe(action)
	third := guard.Observe(action)
	if !first.Allowed || !second.Allowed || third.Allowed {
		t.Fatalf("decisions = %#v %#v %#v", first, second, third)
	}
	if third.Reason != RunGuardRepeatedAction || third.Occurrences != 3 || third.Limit != 2 {
		t.Fatalf("third decision = %#v", third)
	}

	progressed := action
	progressed.TargetRevision = "r2"
	decision := guard.Observe(progressed)
	if !decision.Allowed || decision.Occurrences != 1 {
		t.Fatalf("revision progress was treated as repetition: %#v", decision)
	}
}

func TestRunGuardResetStartsFreshBudget(t *testing.T) {
	guard := NewRunGuard(1)
	action := ActionFingerprintInput{ToolName: "Read", Arguments: json.RawMessage(`{}`)}
	if !guard.Observe(action).Allowed || guard.Observe(action).Allowed {
		t.Fatal("guard did not exhaust configured budget")
	}
	guard.Reset()
	decision := guard.Observe(action)
	if !decision.Allowed || decision.Occurrences != 1 {
		t.Fatalf("decision after reset = %#v", decision)
	}
}

func TestRunGuardOnlyTreatsChangedSuccessfulResultAsProgress(t *testing.T) {
	guard := NewRunGuard(2)
	input := ActionFingerprintInput{
		ToolName:  "Read",
		Arguments: json.RawMessage(`{"file_path":"same.txt"}`),
	}
	if decision := guard.Observe(input); !decision.Allowed {
		t.Fatalf("first observation = %#v", decision)
	}
	if progressed := guard.ObserveResult(input, "version-one", false); !progressed {
		t.Fatal("first successful result was not treated as progress")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if decision := guard.Observe(input); !decision.Allowed {
			t.Fatalf("replay %d blocked too early: %#v", attempt, decision)
		}
		if progressed := guard.ObserveResult(input, "version-one", false); progressed {
			t.Fatalf("identical successful replay %d reset the guard", attempt)
		}
	}
	if decision := guard.Observe(input); decision.Allowed || decision.Reason != RunGuardRepeatedAction {
		t.Fatalf("identical successful replay was not bounded: %#v", decision)
	}

	guard.Reset()
	if decision := guard.Observe(input); !decision.Allowed {
		t.Fatalf("observation after reset = %#v", decision)
	}
	if progressed := guard.ObserveResult(input, "version-two", false); !progressed {
		t.Fatal("changed successful result was not treated as progress")
	}
	if progressed := guard.ObserveResult(input, "ignored-error", true); progressed {
		t.Fatal("failed result was treated as progress")
	}
}

func TestNewRunGuardUsesDefaultLimit(t *testing.T) {
	guard := NewRunGuard(0)
	action := ActionFingerprintInput{ToolName: "Read", Arguments: json.RawMessage(`{}`)}
	for i := 0; i < DefaultRunGuardRepeatLimit; i++ {
		if decision := guard.Observe(action); !decision.Allowed {
			t.Fatalf("observation %d unexpectedly blocked: %#v", i+1, decision)
		}
	}
	if decision := guard.Observe(action); decision.Allowed {
		t.Fatalf("default limit did not block: %#v", decision)
	}
}

func TestRunGuardConcurrentObserve(t *testing.T) {
	const attempts = 32
	guard := NewRunGuard(attempts)
	action := ActionFingerprintInput{ToolName: "Read", Arguments: json.RawMessage(`{"file_path":"a.go"}`)}

	var wait sync.WaitGroup
	wait.Add(attempts)
	decisions := make(chan RunGuardDecision, attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			defer wait.Done()
			decisions <- guard.Observe(action)
		}()
	}
	wait.Wait()
	close(decisions)

	seenOccurrences := make(map[int]bool, attempts)
	for decision := range decisions {
		if !decision.Allowed {
			t.Fatalf("in-budget concurrent decision blocked: %#v", decision)
		}
		seenOccurrences[decision.Occurrences] = true
	}
	if len(seenOccurrences) != attempts {
		t.Fatalf("occurrence counters were not serialized: %#v", seenOccurrences)
	}
	if decision := guard.Observe(action); decision.Allowed || decision.Occurrences != attempts+1 {
		t.Fatalf("post-budget decision = %#v", decision)
	}
}
