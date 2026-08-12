package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func boundCompletionEvidenceLedger(t *testing.T, runID string) *EvidenceLedger {
	t.Helper()
	ledger := NewEvidenceLedger("complete implementation", EvidencePolicyConfig{Policy: EvidencePolicyBlock})
	if err := ledger.BindCompletionRun(runID); err != nil {
		t.Fatalf("BindCompletionRun() error = %v", err)
	}
	return ledger
}

func observeCompletionTool(t *testing.T, ledger *EvidenceLedger, outcome CompletionToolOutcome) CompletionEvidenceRef {
	t.Helper()
	ref, err := ledger.ObserveCompletionToolOutcome(outcome)
	if err != nil {
		t.Fatalf("ObserveCompletionToolOutcome() error = %v", err)
	}
	return ref
}

func TestCompletionEvidenceResolverRequiresExactRunAndSuccess(t *testing.T) {
	const runID = "run-token=raw-run-id-secret"
	ledger := boundCompletionEvidenceLedger(t, runID)
	input := json.RawMessage(`{"command":"go test ./... --token raw-input-secret"}`)
	result := "ok raw-result-secret"
	passed := observeCompletionTool(t, ledger, CompletionToolOutcome{
		ToolName: "Bash",
		Input:    input,
		Result:   result,
		Executed: true,
	})
	failed := observeCompletionTool(t, ledger, CompletionToolOutcome{
		ToolName: "Lint",
		Input:    json.RawMessage(`{"path":"secret-source.go"}`),
		Result:   "failed raw-failure-secret",
		IsError:  true,
		Executed: true,
	})

	wantPassedDigest := canonicalCompletionEvidenceDigest(completionEvidenceDigestInput{
		RunID:        runID,
		Sequence:     1,
		Source:       CompletionEvidenceSourceTool,
		ToolName:     "Bash",
		InputSHA256:  completionEvidenceSHA256(input),
		ResultSHA256: completionEvidenceSHA256([]byte(result)),
		Succeeded:    true,
	})
	if passed.Digest != wantPassedDigest || !validCompletionEvidenceDigest(passed.Digest) {
		t.Fatalf("passed digest = %q, want canonical %q", passed.Digest, wantPassedDigest)
	}

	tests := []struct {
		name   string
		runID  string
		digest string
		want   bool
	}{
		{name: "exact successful ref", runID: runID, digest: passed.Digest, want: true},
		{name: "wrong run", runID: "run-other", digest: passed.Digest},
		{name: "missing digest", runID: runID, digest: completionEvidenceSHA256([]byte("missing"))},
		{name: "failed ref", runID: runID, digest: failed.Digest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ledger.ResolveCompletionEvidence(test.runID, test.digest)
			if err != nil {
				t.Fatalf("ResolveCompletionEvidence() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("ResolveCompletionEvidence() = %v, want %v", got, test.want)
			}
		})
	}

	snapshot := ledger.CompletionEvidenceSnapshot()
	if len(snapshot.Refs) != 2 || snapshot.Refs[1].Succeeded {
		t.Fatalf("failed evidence is not observable: %#v", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"raw-run-id-secret",
		"raw-input-secret",
		"raw-result-secret",
		"secret-source.go",
		"raw-failure-secret",
		`"input"`,
		`"result"`,
		`"command"`,
		`"preview"`,
	} {
		if strings.Contains(string(encoded), raw) {
			t.Fatalf("completion snapshot leaked %q: %s", raw, encoded)
		}
	}
	snapshot.Refs[0].Digest = "caller-mutation"
	if ledger.CompletionEvidenceSnapshot().Refs[0].Digest != passed.Digest {
		t.Fatal("caller mutated ledger through CompletionEvidenceSnapshot")
	}
}

func TestCompletionEvidenceResolverComposesWithCompletionContract(t *testing.T) {
	const runID = "run-contract-composition"
	ledger := boundCompletionEvidenceLedger(t, runID)
	ref := observeCompletionTool(t, ledger, CompletionToolOutcome{
		ToolName: "Bash",
		Input:    json.RawMessage(`{"command":"go test ./internal/agent"}`),
		Result:   "ok",
		Executed: true,
	})
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "finish with evidence",
		DefinitionOfDone: []string{"focused tests pass"},
		Revision:         1,
	})
	if err != nil {
		t.Fatal(err)
	}
	contract, err := NewCompletionContract(CompletionContractSpec{RunID: runID, PlanAnchor: anchor})
	if err != nil {
		t.Fatal(err)
	}
	criterionID, err := CompletionCriterionID("focused tests pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.ApplyClaims(0, []CompletionClaim{{
		CriterionID:    criterionID,
		State:          CompletionClaimSatisfied,
		EvidenceDigest: ref.Digest,
		Summary:        "focused agent tests passed",
	}}, ledger.ResolveCompletionEvidence); err != nil {
		t.Fatalf("ApplyClaims() with ledger resolver error = %v", err)
	}
	if contract.Status() != CompletionStatusComplete {
		t.Fatalf("contract status = %q, want complete", contract.Status())
	}
}

func TestCompletionEvidenceExcludesNonExecutionAndReportCompletion(t *testing.T) {
	ledger := boundCompletionEvidenceLedger(t, "run-exclusions")
	tests := []struct {
		name    string
		outcome CompletionToolOutcome
	}{
		{name: "not executed", outcome: CompletionToolOutcome{ToolName: "Bash"}},
		{name: "synthetic", outcome: CompletionToolOutcome{ToolName: "Bash", Executed: true, Synthetic: true}},
		{name: "denied", outcome: CompletionToolOutcome{ToolName: "Bash", Executed: true, Denied: true}},
		{name: "report completion", outcome: CompletionToolOutcome{ToolName: "ReportCompletion", Executed: true}},
		{name: "case folded report completion", outcome: CompletionToolOutcome{ToolName: "reportcompletion", Executed: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ref, err := ledger.ObserveCompletionToolOutcome(test.outcome)
			if err != nil {
				t.Fatalf("ignored outcome error = %v", err)
			}
			if ref != (CompletionEvidenceRef{}) {
				t.Fatalf("ignored outcome returned ref %#v", ref)
			}
		})
	}
	if got := ledger.CompletionEvidenceSnapshot().Refs; len(got) != 0 {
		t.Fatalf("excluded outcomes appended refs: %#v", got)
	}
}

func TestCompletionEvidenceAutoVerifyAndDuplicateAppendOrder(t *testing.T) {
	const runID = "run-order"
	ledger := boundCompletionEvidenceLedger(t, runID)
	outcome := CompletionToolOutcome{
		ToolName: "Bash",
		Input:    json.RawMessage(`{"command":"go test ./internal/agent"}`),
		Result:   "ok",
		Executed: true,
	}
	first := observeCompletionTool(t, ledger, outcome)
	second := observeCompletionTool(t, ledger, outcome)
	ledger.ObserveAutoVerify("auto failure raw-output", true, true)
	ledger.ObserveAutoVerify("not run raw-output", false, false)
	ledger.ObserveAutoVerify("auto success raw-output", false, true)

	snapshot := ledger.CompletionEvidenceSnapshot()
	if snapshot.RunID != runID || len(snapshot.Refs) != 4 {
		t.Fatalf("snapshot = %#v, want four append-ordered refs", snapshot)
	}
	if first.Digest == second.Digest {
		t.Fatal("duplicate executed outcomes were incorrectly deduplicated")
	}
	wantSources := []CompletionEvidenceSource{
		CompletionEvidenceSourceTool,
		CompletionEvidenceSourceTool,
		CompletionEvidenceSourceAutoVerify,
		CompletionEvidenceSourceAutoVerify,
	}
	for index, ref := range snapshot.Refs {
		if ref.Sequence != uint64(index+1) || ref.Source != wantSources[index] {
			t.Fatalf("ref[%d] = %#v, want sequence=%d source=%q", index, ref, index+1, wantSources[index])
		}
	}
	if snapshot.Refs[2].Succeeded || !snapshot.Refs[3].Succeeded {
		t.Fatalf("auto-verify success states = %#v", snapshot.Refs[2:])
	}
	failedResolved, _ := ledger.ResolveCompletionEvidence(runID, snapshot.Refs[2].Digest)
	passedResolved, _ := ledger.ResolveCompletionEvidence(runID, snapshot.Refs[3].Digest)
	if failedResolved || !passedResolved {
		t.Fatalf("auto-verify resolver states = failed:%v passed:%v", failedResolved, passedResolved)
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "raw-output") {
		t.Fatalf("auto-verify raw output leaked: %s", encoded)
	}
}

func TestCompletionEvidenceBindingAndFieldBounds(t *testing.T) {
	unbound := NewEvidenceLedger("complete", EvidencePolicyConfig{})
	_, err := unbound.ObserveCompletionToolOutcome(CompletionToolOutcome{ToolName: "Bash", Executed: true})
	if !errors.Is(err, errCompletionEvidenceNotBound) {
		t.Fatalf("unbound observation error = %v, want not-bound", err)
	}
	if resolved, resolveErr := unbound.ResolveCompletionEvidence("run", completionEvidenceSHA256(nil)); resolveErr != nil || resolved {
		t.Fatalf("unbound resolver = (%v, %v), want (false, nil)", resolved, resolveErr)
	}

	invalidRunIDs := []string{
		"",
		" run",
		"run\ncontrol",
		strings.Repeat("r", maxCompletionRunIDBytes+1),
	}
	for _, runID := range invalidRunIDs {
		ledger := NewEvidenceLedger("complete", EvidencePolicyConfig{})
		if err := ledger.BindCompletionRun(runID); !errors.Is(err, errCompletionEvidenceInvalidRunID) {
			t.Fatalf("BindCompletionRun(%q) error = %v, want invalid run ID", runID, err)
		}
	}

	ledger := boundCompletionEvidenceLedger(t, "run-bound")
	if err := ledger.BindCompletionRun("run-bound"); err != nil {
		t.Fatalf("same-run bind is not idempotent: %v", err)
	}
	if err := ledger.BindCompletionRun("run-other"); !errors.Is(err, errCompletionEvidenceRunBound) {
		t.Fatalf("conflicting bind error = %v, want already-bound", err)
	}

	invalidTools := []string{"", " Bash", "unsafe tool", "Bash;rm", strings.Repeat("t", maxCompletionEvidenceToolNameLen+1)}
	for _, toolName := range invalidTools {
		_, err := ledger.ObserveCompletionToolOutcome(CompletionToolOutcome{ToolName: toolName, Executed: true})
		if !errors.Is(err, errCompletionEvidenceInvalidTool) {
			t.Fatalf("tool %q error = %v, want invalid tool", toolName, err)
		}
	}
	_, err = ledger.ObserveCompletionToolOutcome(CompletionToolOutcome{
		ToolName: "Bash",
		Input:    json.RawMessage(strings.Repeat("i", maxCompletionEvidenceInputBytes+1)),
		Executed: true,
	})
	if !errors.Is(err, errCompletionEvidenceFieldLarge) {
		t.Fatalf("oversized input error = %v", err)
	}
	_, err = ledger.ObserveCompletionToolOutcome(CompletionToolOutcome{
		ToolName: "Bash",
		Result:   strings.Repeat("r", maxCompletionEvidenceResultBytes+1),
		Executed: true,
	})
	if !errors.Is(err, errCompletionEvidenceFieldLarge) {
		t.Fatalf("oversized result error = %v", err)
	}
	if len(ledger.CompletionEvidenceSnapshot().Refs) != 0 {
		t.Fatal("rejected fields mutated the completion evidence index")
	}
}

func TestCompletionEvidenceReferenceLimitFailsClosed(t *testing.T) {
	ledger := boundCompletionEvidenceLedger(t, "run-limit")
	for index := 0; index < maxCompletionEvidenceRefs; index++ {
		observeCompletionTool(t, ledger, CompletionToolOutcome{
			ToolName: "Bash",
			Input:    json.RawMessage(fmt.Sprintf(`{"index":%d}`, index)),
			Result:   fmt.Sprintf("result-%d", index),
			Executed: true,
		})
	}
	_, err := ledger.ObserveCompletionToolOutcome(CompletionToolOutcome{ToolName: "Bash", Executed: true})
	if !errors.Is(err, errCompletionEvidenceLimit) {
		t.Fatalf("reference overflow error = %v, want limit", err)
	}
	snapshot := ledger.CompletionEvidenceSnapshot()
	if len(snapshot.Refs) != maxCompletionEvidenceRefs || snapshot.Refs[len(snapshot.Refs)-1].Sequence != maxCompletionEvidenceRefs {
		t.Fatalf("bounded snapshot size/order = %d/%d", len(snapshot.Refs), snapshot.Refs[len(snapshot.Refs)-1].Sequence)
	}
}

func TestCompletionEvidenceConcurrentObservationIsRaceSafeAndOrdered(t *testing.T) {
	const count = 64
	ledger := boundCompletionEvidenceLedger(t, "run-concurrent")
	start := make(chan struct{})
	errs := make(chan error, count)
	var ready sync.WaitGroup
	ready.Add(count)
	for index := 0; index < count; index++ {
		index := index
		go func() {
			ready.Done()
			<-start
			_, err := ledger.ObserveCompletionToolOutcome(CompletionToolOutcome{
				ToolName: "Bash",
				Input:    json.RawMessage(fmt.Sprintf(`{"index":%d}`, index)),
				Result:   fmt.Sprintf("result-%d", index),
				Executed: true,
			})
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	for index := 0; index < count; index++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent observation error = %v", err)
		}
	}

	snapshot := ledger.CompletionEvidenceSnapshot()
	if len(snapshot.Refs) != count {
		t.Fatalf("concurrent refs = %d, want %d", len(snapshot.Refs), count)
	}
	seen := make(map[string]struct{}, count)
	for index, ref := range snapshot.Refs {
		if ref.Sequence != uint64(index+1) {
			t.Fatalf("ref[%d] sequence = %d, want %d", index, ref.Sequence, index+1)
		}
		if _, duplicate := seen[ref.Digest]; duplicate {
			t.Fatalf("duplicate concurrent digest %q", ref.Digest)
		}
		seen[ref.Digest] = struct{}{}
		resolved, err := ledger.ResolveCompletionEvidence("run-concurrent", ref.Digest)
		if err != nil || !resolved {
			t.Fatalf("concurrent ref did not resolve: (%v, %v)", resolved, err)
		}
	}
}

func TestCompletionEvidencePreservesVerificationLedgerByteAndSemanticBehavior(t *testing.T) {
	legacy := NewEvidenceLedger("complete implementation", EvidencePolicyConfig{Policy: EvidencePolicyBlock})
	indexed := NewEvidenceLedger("complete implementation", EvidencePolicyConfig{Policy: EvidencePolicyBlock})
	if err := indexed.BindCompletionRun("run-parity"); err != nil {
		t.Fatal(err)
	}
	observeLegacy := func(ledger *EvidenceLedger) {
		ledger.ObserveChangedFile("internal/service.go")
		ledger.ObserveToolResult(
			"Bash",
			json.RawMessage(`{"command":"go test ./internal/..."}`),
			"ok",
			false,
		)
		ledger.ObserveAutoVerify("ok", false, true)
		ledger.MarkStopBlock()
	}
	observeLegacy(legacy)
	observeLegacy(indexed)
	for _, ledger := range []*EvidenceLedger{legacy, indexed} {
		ledger.mu.Lock()
		for index := range ledger.Records {
			ledger.Records[index].At = ""
		}
		ledger.mu.Unlock()
	}

	legacyLedgerJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	indexedLedgerJSON, err := json.Marshal(indexed)
	if err != nil {
		t.Fatal(err)
	}
	if string(legacyLedgerJSON) != string(indexedLedgerJSON) {
		t.Fatalf("public verification ledger bytes changed:\nlegacy=%s\nindexed=%s", legacyLedgerJSON, indexedLedgerJSON)
	}
	legacyGateJSON, _ := json.Marshal(legacy.Evaluate())
	indexedGateJSON, _ := json.Marshal(indexed.Evaluate())
	if string(legacyGateJSON) != string(indexedGateJSON) {
		t.Fatalf("Evaluate semantics changed:\nlegacy=%s\nindexed=%s", legacyGateJSON, indexedGateJSON)
	}
	legacyReceiptJSON, _ := json.Marshal(legacy.ApplyToReceipt(ReceiptVerification{}))
	indexedReceiptJSON, _ := json.Marshal(indexed.ApplyToReceipt(ReceiptVerification{}))
	if string(legacyReceiptJSON) != string(indexedReceiptJSON) {
		t.Fatalf("receipt semantics changed:\nlegacy=%s\nindexed=%s", legacyReceiptJSON, indexedReceiptJSON)
	}
	if len(legacy.CompletionEvidenceSnapshot().Refs) != 0 || len(indexed.CompletionEvidenceSnapshot().Refs) != 1 {
		t.Fatalf("completion index did not remain private from legacy behavior")
	}
}
