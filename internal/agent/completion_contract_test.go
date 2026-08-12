package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

func completionTestAnchor(t *testing.T, definitionOfDone []string) PlanAnchor {
	t.Helper()
	anchor, err := NewPlanAnchor(PlanAnchorSpec{
		Objective:        "finish the run truthfully",
		DefinitionOfDone: definitionOfDone,
		Revision:         3,
	})
	if err != nil {
		t.Fatalf("NewPlanAnchor() error = %v", err)
	}
	return anchor
}

func completionTestDigest(label string) string {
	digest := sha256.Sum256([]byte(label))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func requireCompletionError(t *testing.T, err error, want CompletionContractErrorCode) *CompletionContractError {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %q", want)
	}
	var typed *CompletionContractError
	if !errors.As(err, &typed) {
		t.Fatalf("error type = %T, want *CompletionContractError: %v", err, err)
	}
	if typed.Code != want {
		t.Fatalf("error code = %q, want %q", typed.Code, want)
	}
	return typed
}

func TestCompletionContractHappyPathRequiresResolvedEvidence(t *testing.T) {
	anchor := completionTestAnchor(t, []string{
		"focused tests pass",
		"manual release note reviewed",
	})
	contract, err := NewCompletionContract(CompletionContractSpec{
		RunID:                       "run-happy-1",
		PlanAnchor:                  anchor,
		EvidenceNotRequiredCriteria: []string{" manual   release note reviewed "},
	})
	if err != nil {
		t.Fatalf("NewCompletionContract() error = %v", err)
	}
	testsID, _ := CompletionCriterionID("focused tests pass")
	manualID, _ := CompletionCriterionID("manual release note reviewed")
	evidenceDigest := completionTestDigest("go test ./internal/agent")
	resolverCalls := 0

	err = contract.ApplyClaims(0, []CompletionClaim{
		{
			CriterionID:    testsID,
			State:          CompletionClaimSatisfied,
			EvidenceDigest: evidenceDigest,
			Summary:        "go test passed; token=raw-secret-value",
		},
		{
			CriterionID: manualID,
			State:       CompletionClaimSatisfied,
			Assertion:   "reviewed against the scoped diff",
		},
	}, func(runID, digest string) (bool, error) {
		resolverCalls++
		if runID != "run-happy-1" || digest != evidenceDigest {
			t.Fatalf("resolver got run=%q digest=%q", runID, digest)
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("ApplyClaims() error = %v", err)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolverCalls)
	}
	if got := contract.Revision(); got != 1 {
		t.Fatalf("Revision() = %d, want 1", got)
	}
	if got := contract.Status(); got != CompletionStatusComplete {
		t.Fatalf("Status() = %q, want %q", got, CompletionStatusComplete)
	}

	snapshot, err := contract.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.Status != CompletionStatusComplete || len(snapshot.Criteria) != 2 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if !snapshot.Criteria[0].EvidenceRequired || snapshot.Criteria[1].EvidenceRequired {
		t.Fatalf("evidence requirements = %#v", snapshot.Criteria)
	}
	if snapshot.Criteria[0].EvidenceDigest != evidenceDigest || snapshot.Criteria[0].ClaimRevision != 1 {
		t.Fatalf("evidenced criterion = %#v", snapshot.Criteria[0])
	}
	if strings.Contains(snapshot.Criteria[0].Summary, "raw-secret-value") || !strings.Contains(snapshot.Criteria[0].Summary, "[REDACTED]") {
		t.Fatalf("summary was not redacted: %q", snapshot.Criteria[0].Summary)
	}
}

func TestCompletionContractRejectsFalseDoneClaims(t *testing.T) {
	contract, err := NewCompletionContract(CompletionContractSpec{
		RunID:      "run-false-done",
		PlanAnchor: completionTestAnchor(t, []string{"tests pass"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	criterionID, _ := CompletionCriterionID("tests pass")
	digest := completionTestDigest("missing")

	requireCompletionError(t, contract.ApplyClaims(0, []CompletionClaim{{
		CriterionID: criterionID,
		State:       CompletionClaimSatisfied,
		Summary:     "the model says it is done",
	}}, nil), CompletionErrorEvidenceRequired)
	requireCompletionError(t, contract.ApplyClaims(0, []CompletionClaim{{
		CriterionID:    criterionID,
		State:          CompletionClaimSatisfied,
		EvidenceDigest: digest,
	}}, nil), CompletionErrorEvidenceResolver)
	requireCompletionError(t, contract.ApplyClaims(0, []CompletionClaim{{
		CriterionID:    criterionID,
		State:          CompletionClaimSatisfied,
		EvidenceDigest: digest,
	}}, func(_, _ string) (bool, error) {
		return false, nil
	}), CompletionErrorEvidenceMismatch)

	if contract.Revision() != 0 || contract.Status() != CompletionStatusIncomplete {
		t.Fatalf("false done changed contract: revision=%d status=%q", contract.Revision(), contract.Status())
	}
}

func TestCompletionContractBlockedTransitionAndRecovery(t *testing.T) {
	contract, err := NewCompletionContract(CompletionContractSpec{
		RunID:      "run-blocked",
		PlanAnchor: completionTestAnchor(t, []string{"integration tests pass"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	criterionID, _ := CompletionCriterionID("integration tests pass")
	if err := contract.ApplyClaims(0, []CompletionClaim{{
		CriterionID: criterionID,
		State:       CompletionClaimBlocked,
		Summary:     "required service is unavailable",
	}}, nil); err != nil {
		t.Fatalf("block claim error = %v", err)
	}
	if contract.Status() != CompletionStatusBlocked || contract.Revision() != 1 {
		t.Fatalf("blocked state = (%q, %d)", contract.Status(), contract.Revision())
	}

	digest := completionTestDigest("integration passed")
	if err := contract.ApplyClaims(1, []CompletionClaim{{
		CriterionID:    criterionID,
		State:          CompletionClaimSatisfied,
		EvidenceDigest: digest,
		Summary:        "integration suite passed",
	}}, func(runID, got string) (bool, error) {
		return runID == "run-blocked" && got == digest, nil
	}); err != nil {
		t.Fatalf("recovery claim error = %v", err)
	}
	if contract.Status() != CompletionStatusComplete || contract.Revision() != 2 {
		t.Fatalf("recovered state = (%q, %d)", contract.Status(), contract.Revision())
	}
	snapshot, _ := contract.Snapshot()
	if snapshot.Criteria[0].ClaimRevision != 2 {
		t.Fatalf("claim revision = %d, want 2", snapshot.Criteria[0].ClaimRevision)
	}
}

func TestCompletionContractRevisionCASRejectsConcurrentStaleClaim(t *testing.T) {
	contract, err := NewCompletionContract(CompletionContractSpec{
		RunID:                       "run-cas",
		PlanAnchor:                  completionTestAnchor(t, []string{"operator assertion"}),
		EvidenceNotRequiredCriteria: []string{"operator assertion"},
	})
	if err != nil {
		t.Fatal(err)
	}
	criterionID, _ := CompletionCriterionID("operator assertion")
	claims := []CompletionClaim{
		{CriterionID: criterionID, State: CompletionClaimSatisfied, Assertion: "confirmed"},
		{CriterionID: criterionID, State: CompletionClaimBlocked, Summary: "not confirmed"},
	}

	start := make(chan struct{})
	results := make(chan error, len(claims))
	var ready sync.WaitGroup
	ready.Add(len(claims))
	for _, claim := range claims {
		claim := claim
		go func() {
			ready.Done()
			<-start
			results <- contract.ApplyClaims(0, []CompletionClaim{claim}, nil)
		}()
	}
	ready.Wait()
	close(start)

	successes := 0
	stale := 0
	for range claims {
		err := <-results
		if err == nil {
			successes++
			continue
		}
		var typed *CompletionContractError
		if errors.As(err, &typed) && typed.Code == CompletionErrorStaleRevision {
			stale++
			continue
		}
		t.Fatalf("unexpected concurrent error: %v", err)
	}
	if successes != 1 || stale != 1 || contract.Revision() != 1 {
		t.Fatalf("success=%d stale=%d revision=%d", successes, stale, contract.Revision())
	}

	staleBefore, _ := contract.SnapshotJSON()
	requireCompletionError(t, contract.ApplyClaims(0, []CompletionClaim{{
		CriterionID: criterionID,
		State:       CompletionClaimPending,
	}}, nil), CompletionErrorStaleRevision)
	staleAfter, _ := contract.SnapshotJSON()
	if string(staleBefore) != string(staleAfter) {
		t.Fatal("stale claim mutated the contract")
	}
}

func TestCompletionContractRejectsUnknownDuplicateAndConflictingClaims(t *testing.T) {
	contract, err := NewCompletionContract(CompletionContractSpec{
		RunID:                       "run-claim-errors",
		PlanAnchor:                  completionTestAnchor(t, []string{"manual check", "second check"}),
		EvidenceNotRequiredCriteria: []string{"manual check", "second check"},
	})
	if err != nil {
		t.Fatal(err)
	}
	criterionID, _ := CompletionCriterionID("manual check")
	unknownID, _ := CompletionCriterionID("unknown check")

	requireCompletionError(t, contract.ApplyClaims(0, []CompletionClaim{{
		CriterionID: unknownID,
		State:       CompletionClaimSatisfied,
		Summary:     "unknown",
	}}, nil), CompletionErrorUnknownCriterion)
	duplicate := CompletionClaim{CriterionID: criterionID, State: CompletionClaimSatisfied, Assertion: "confirmed"}
	requireCompletionError(t, contract.ApplyClaims(0, []CompletionClaim{duplicate, duplicate}, nil), CompletionErrorDuplicateClaim)
	requireCompletionError(t, contract.ApplyClaims(0, []CompletionClaim{
		duplicate,
		{CriterionID: criterionID, State: CompletionClaimBlocked, Summary: "blocked"},
	}, nil), CompletionErrorConflictingClaim)

	if err := contract.ApplyClaims(0, []CompletionClaim{duplicate}, nil); err != nil {
		t.Fatal(err)
	}
	requireCompletionError(t, contract.ApplyClaims(1, []CompletionClaim{duplicate}, nil), CompletionErrorDuplicateClaim)
	requireCompletionError(t, contract.ApplyClaims(1, []CompletionClaim{{
		CriterionID: criterionID,
		State:       CompletionClaimSatisfied,
		Assertion:   "different assertion",
	}}, nil), CompletionErrorConflictingClaim)
}

func TestCompletionContractEvidenceValidationAndResolverMismatch(t *testing.T) {
	contract, err := NewCompletionContract(CompletionContractSpec{
		RunID:      "run-evidence-errors",
		PlanAnchor: completionTestAnchor(t, []string{"tests pass"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	criterionID, _ := CompletionCriterionID("tests pass")
	for _, digest := range []string{
		"sha256:short",
		"md5:00000000000000000000000000000000",
		"sha256:" + strings.Repeat("A", sha256.Size*2),
		"sha256:" + strings.Repeat("z", sha256.Size*2),
	} {
		requireCompletionError(t, contract.ApplyClaims(0, []CompletionClaim{{
			CriterionID:    criterionID,
			State:          CompletionClaimSatisfied,
			EvidenceDigest: digest,
		}}, func(_, _ string) (bool, error) { return true, nil }), CompletionErrorInvalidEvidence)
	}

	resolverDigest := completionTestDigest("resolver failure")
	requireCompletionError(t, contract.ApplyClaims(0, []CompletionClaim{{
		CriterionID:    criterionID,
		State:          CompletionClaimSatisfied,
		EvidenceDigest: resolverDigest,
	}}, func(_, _ string) (bool, error) {
		return false, errors.New("raw resolver detail must not cross the boundary")
	}), CompletionErrorEvidenceResolver)
	if contract.Revision() != 0 {
		t.Fatalf("evidence failure advanced revision to %d", contract.Revision())
	}
}

func TestCompletionContractFailedBatchDoesNotMutateAnyCriterion(t *testing.T) {
	contract, err := NewCompletionContract(CompletionContractSpec{
		RunID:                       "run-no-mutation",
		PlanAnchor:                  completionTestAnchor(t, []string{"manual check", "evidenced check"}),
		EvidenceNotRequiredCriteria: []string{"manual check"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manualID, _ := CompletionCriterionID("manual check")
	unknownID, _ := CompletionCriterionID("unknown")
	before, _ := contract.SnapshotJSON()
	resolverCalls := 0

	err = contract.ApplyClaims(0, []CompletionClaim{
		{CriterionID: manualID, State: CompletionClaimSatisfied, Assertion: "confirmed"},
		{CriterionID: unknownID, State: CompletionClaimSatisfied, Assertion: "unknown"},
	}, func(_, _ string) (bool, error) {
		resolverCalls++
		return true, nil
	})
	requireCompletionError(t, err, CompletionErrorUnknownCriterion)
	after, _ := contract.SnapshotJSON()
	if string(before) != string(after) || contract.Revision() != 0 {
		t.Fatalf("failed batch mutated contract:\nbefore=%s\nafter=%s", before, after)
	}
	if resolverCalls != 0 {
		t.Fatalf("resolver called before structural validation completed: %d", resolverCalls)
	}
}

func TestCompletionContractStableIDsImmutableTextAndReceiptSafeJSON(t *testing.T) {
	criteria := []string{
		" tests   pass ",
		"secret token=criterion-secret-value is rotated",
	}
	anchor := completionTestAnchor(t, criteria)
	criteria[0] = "caller mutation"
	contract, err := NewCompletionContract(CompletionContractSpec{
		RunID:      "run-snapshot",
		PlanAnchor: anchor,
	})
	if err != nil {
		t.Fatal(err)
	}

	idFromWhitespace, err := CompletionCriterionID("  tests   pass  ")
	if err != nil {
		t.Fatal(err)
	}
	idNormalized, _ := CompletionCriterionID("tests pass")
	if idFromWhitespace != idNormalized {
		t.Fatalf("stable IDs differ: %q != %q", idFromWhitespace, idNormalized)
	}
	if text, ok := contract.CriterionText(idNormalized); !ok || text != "tests pass" {
		t.Fatalf("CriterionText() = (%q, %v)", text, ok)
	}

	reordered, err := NewCompletionContract(CompletionContractSpec{
		RunID:      "run-reordered",
		PlanAnchor: completionTestAnchor(t, []string{"secret token=criterion-secret-value is rotated", "tests pass"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if text, ok := reordered.CriterionText(idNormalized); !ok || text != "tests pass" {
		t.Fatalf("reordered criterion ID changed: (%q, %v)", text, ok)
	}

	first, err := contract.SnapshotJSON()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := contract.SnapshotJSON()
	marshaled, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || string(first) != string(marshaled) {
		t.Fatalf("snapshot JSON is not deterministic:\n%s\n%s\n%s", first, second, marshaled)
	}
	if strings.Contains(string(first), "criterion-secret-value") || !strings.Contains(string(first), "[REDACTED]") {
		t.Fatalf("snapshot leaked criterion secret: %s", first)
	}
	if strings.Contains(string(first), "payload") {
		t.Fatalf("snapshot contains a raw evidence payload field: %s", first)
	}
}

func TestCompletionContractRejectsEmptyAndOversizedFields(t *testing.T) {
	anchor := completionTestAnchor(t, []string{"manual check"})
	tests := []struct {
		name string
		spec CompletionContractSpec
		code CompletionContractErrorCode
	}{
		{"empty run", CompletionContractSpec{PlanAnchor: anchor}, CompletionErrorInvalidField},
		{"oversized run", CompletionContractSpec{RunID: strings.Repeat("r", maxCompletionRunIDBytes+1), PlanAnchor: anchor}, CompletionErrorFieldTooLarge},
		{"empty optional criterion", CompletionContractSpec{RunID: "run", PlanAnchor: anchor, EvidenceNotRequiredCriteria: []string{" "}}, CompletionErrorInvalidField},
		{"unknown optional criterion", CompletionContractSpec{RunID: "run", PlanAnchor: anchor, EvidenceNotRequiredCriteria: []string{"unknown"}}, CompletionErrorUnknownCriterion},
		{"duplicate optional criterion", CompletionContractSpec{RunID: "run", PlanAnchor: anchor, EvidenceNotRequiredCriteria: []string{"manual check", " manual  check "}}, CompletionErrorDuplicateCriterion},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewCompletionContract(test.spec)
			requireCompletionError(t, err, test.code)
		})
	}

	hugeAnchor := completionTestAnchor(t, []string{strings.Repeat("x", maxCompletionCriterionTextBytes+1)})
	_, err := NewCompletionContract(CompletionContractSpec{RunID: "run", PlanAnchor: hugeAnchor})
	requireCompletionError(t, err, CompletionErrorFieldTooLarge)

	contract, err := NewCompletionContract(CompletionContractSpec{
		RunID:                       "run-fields",
		PlanAnchor:                  anchor,
		EvidenceNotRequiredCriteria: []string{"manual check"},
	})
	if err != nil {
		t.Fatal(err)
	}
	criterionID, _ := CompletionCriterionID("manual check")
	claimTests := []struct {
		name  string
		claim CompletionClaim
		code  CompletionContractErrorCode
	}{
		{"empty ID", CompletionClaim{State: CompletionClaimSatisfied, Assertion: "yes"}, CompletionErrorInvalidField},
		{"empty state", CompletionClaim{CriterionID: criterionID, Assertion: "yes"}, CompletionErrorInvalidClaimState},
		{"oversized summary", CompletionClaim{CriterionID: criterionID, State: CompletionClaimSatisfied, Summary: strings.Repeat("s", maxCompletionSummaryBytes+1)}, CompletionErrorFieldTooLarge},
		{"empty assertion", CompletionClaim{CriterionID: criterionID, State: CompletionClaimSatisfied}, CompletionErrorInvalidField},
		{"empty blocked reason", CompletionClaim{CriterionID: criterionID, State: CompletionClaimBlocked}, CompletionErrorInvalidField},
	}
	for _, test := range claimTests {
		t.Run(test.name, func(t *testing.T) {
			requireCompletionError(t, contract.ApplyClaims(0, []CompletionClaim{test.claim}, nil), test.code)
		})
	}
	requireCompletionError(t, contract.ApplyClaims(0, nil, nil), CompletionErrorEmptyClaims)
}
