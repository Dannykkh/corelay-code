package agent

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRunOptionsCompletionEvidenceNotRequiredCriteriaCopiesAndPreservesNil(t *testing.T) {
	var zero RunOptions
	if got := zero.CompletionEvidenceNotRequiredCriteria(); got != nil {
		t.Fatalf("zero criteria = %#v, want nil", got)
	}

	opts := RunOptions{CompletionEvidenceNotRequired: []string{"docs-only", "explicit assertion"}}
	copyOne := opts.CompletionEvidenceNotRequiredCriteria()
	if !reflect.DeepEqual(copyOne, opts.CompletionEvidenceNotRequired) {
		t.Fatalf("criteria copy = %#v", copyOne)
	}
	copyOne[1] = "consumer mutation"
	copyTwo := opts.CompletionEvidenceNotRequiredCriteria()
	if !reflect.DeepEqual(copyTwo, []string{"docs-only", "explicit assertion"}) {
		t.Fatalf("defensive copy = %#v", copyTwo)
	}
	if opts.CompletionEvidenceNotRequired[1] != "explicit assertion" {
		t.Fatalf("returned slice mutated options: %#v", opts.CompletionEvidenceNotRequired)
	}
}

func TestAgentReceiptNilCompletionPreservesDefaultJSONSemantics(t *testing.T) {
	receipt := AgentReceipt{
		Version:      1,
		Provider:     "provider",
		Model:        "model",
		EditedFiles:  []string{},
		Verification: ReceiptVerification{Status: "not-run", Source: "none"},
	}
	first, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("nil completion JSON is not deterministic:\n%s\n%s", first, second)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(first, &object); err != nil {
		t.Fatal(err)
	}
	if _, exists := object["completion"]; exists {
		t.Fatalf("nil completion changed receipt bytes: %s", first)
	}
	if object["verification"] == nil || object["editedFiles"] == nil {
		t.Fatalf("legacy receipt fields changed: %s", first)
	}
	if (RunSummary{}).Completion != nil {
		t.Fatal("zero RunSummary completion is not nil")
	}
}

func TestAgentReceiptCompletionSnapshotIsBoundedRedactedDeterministicAndDoesNotMutate(t *testing.T) {
	secret := "sk-" + strings.Repeat("a", 32)
	originalText := strings.Repeat("t", maxCompletionCriterionTextBytes+64) + " password=" + secret
	originalSummary := strings.Repeat("s", maxCompletionSummaryBytes+32) + " token=" + secret
	originalAssertion := "Authorization: Bearer " + strings.Repeat("b", 32)
	snapshot := &CompletionContractSnapshot{
		Version:      1,
		RunID:        "run-" + secret,
		PlanRevision: 7,
		Revision:     11,
		Status:       CompletionStatusComplete,
		Criteria: []CompletionCriterionSnapshot{{
			ID:               "criterion:" + secret,
			Text:             originalText,
			EvidenceRequired: true,
			State:            CompletionClaimSatisfied,
			EvidenceDigest:   secret,
			Summary:          originalSummary,
			Assertion:        originalAssertion,
			ClaimRevision:    3,
		}},
	}
	for index := 1; index <= maxCompletionCriteria; index++ {
		snapshot.Criteria = append(snapshot.Criteria, CompletionCriterionSnapshot{
			ID:    "criterion-filler",
			Text:  "bounded filler",
			State: CompletionClaimPending,
		})
	}
	receipt := AgentReceipt{
		Version:      1,
		Provider:     "provider",
		Model:        "model",
		Verification: ReceiptVerification{Status: "passed", Source: "test"},
		Completion:   snapshot,
	}

	first, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("completion receipt JSON is not deterministic")
	}
	if bytes.Contains(first, []byte(secret)) || bytes.Contains(first, []byte(strings.Repeat("b", 32))) {
		t.Fatalf("completion secret leaked: %s", first)
	}
	if snapshot.Criteria[0].Text != originalText || snapshot.Criteria[0].Summary != originalSummary || snapshot.Criteria[0].Assertion != originalAssertion || snapshot.Criteria[0].EvidenceDigest != secret {
		t.Fatalf("receipt marshal mutated source snapshot: %#v", snapshot.Criteria[0])
	}

	var payload agentReceiptPayload
	if err := json.Unmarshal(first, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Completion == nil || len(payload.Completion.Criteria) != maxCompletionCriteria {
		t.Fatalf("completion payload = %#v", payload.Completion)
	}
	if payload.Verification.TerminalState != EvidenceTerminalVerified {
		t.Fatalf("complete contract changed verified evidence terminal: %q", payload.Verification.TerminalState)
	}
	criterion := payload.Completion.Criteria[0]
	if len(criterion.Text) > maxCompletionCriterionTextBytes || len(criterion.Summary) > maxCompletionSummaryBytes || len(criterion.Assertion) > maxCompletionSummaryBytes {
		t.Fatalf("completion fields are not bounded: text=%d summary=%d assertion=%d", len(criterion.Text), len(criterion.Summary), len(criterion.Assertion))
	}
	if criterion.EvidenceDigest != "" {
		t.Fatalf("invalid evidence digest was retained: %q", criterion.EvidenceDigest)
	}
	payload.Completion.Criteria[0].Text = "mutated copy"
	if snapshot.Criteria[0].Text != originalText {
		t.Fatal("serialized completion aliases source snapshot")
	}
	for _, forbidden := range []string{`"evidencePayload"`, `"toolInput"`, `"toolResult"`} {
		if bytes.Contains(first, []byte(forbidden)) {
			t.Fatalf("completion receipt introduced raw payload field %s", forbidden)
		}
	}
}

func TestAgentReceiptIncompleteAndBlockedCompletionOverrideEvidenceTerminal(t *testing.T) {
	for _, status := range []CompletionContractStatus{CompletionStatusIncomplete, CompletionStatusBlocked} {
		t.Run(string(status), func(t *testing.T) {
			receipt := AgentReceipt{
				Version: 1,
				Verification: ReceiptVerification{
					Status: "passed",
					Source: "test",
					Gate:   EvidenceGateAllow,
				},
				Completion: &CompletionContractSnapshot{Version: 1, Status: status},
			}
			data, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			var payload agentReceiptPayload
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Verification.TerminalState != EvidenceTerminalBlocked {
				t.Fatalf("terminalState = %q, want blocked; receipt=%s", payload.Verification.TerminalState, data)
			}
		})
	}
}

func TestDurableRunObserverLegacyAndTypedDoneTerminalMetadata(t *testing.T) {
	t.Run("legacy nil", func(t *testing.T) {
		o := NewDurableRunObserver("legacy")
		o.Observe(Event{Type: "text", Data: "legacy output"})
		o.Observe(Event{Type: "done"})
		if !o.Completed() || len(o.Messages()) != 1 {
			t.Fatalf("legacy done completion/messages = %v/%#v", o.Completed(), o.Messages())
		}
		if metadata, ok := o.TerminalMetadata(); ok || metadata != (DurableRunTerminalMetadata{}) {
			t.Fatalf("legacy metadata = %#v, ok=%v", metadata, ok)
		}
	})

	t.Run("typed complete", func(t *testing.T) {
		secret := "sk-" + strings.Repeat("z", 32)
		o := NewDurableRunObserver("typed")
		o.Observe(Event{Type: "done", Data: map[string]any{
			"terminalState":       EvidenceTerminalVerified,
			"completionStatus":    CompletionStatusComplete,
			"completionRevision":  uint64(9),
			"completionCriteria":  4,
			"completionSatisfied": 4,
			"completionBlocked":   0,
			"completionEvidence":  secret,
			"toolResult":          secret,
		}})
		metadata, ok := o.TerminalMetadata()
		if !o.Completed() || !ok {
			t.Fatalf("typed done completed=%v metadata=%#v ok=%v", o.Completed(), metadata, ok)
		}
		want := DurableRunTerminalMetadata{
			TerminalState:       EvidenceTerminalVerified,
			CompletionStatus:    CompletionStatusComplete,
			CompletionRevision:  9,
			CompletionCriteria:  4,
			CompletionSatisfied: 4,
		}
		if metadata != want {
			t.Fatalf("metadata = %#v, want %#v", metadata, want)
		}
		data, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte("completionEvidence")) || bytes.Contains(data, []byte("toolResult")) {
			t.Fatalf("content leaked through terminal accessor: %s", data)
		}
	})

	t.Run("typed blocked remains committable but not successful", func(t *testing.T) {
		o := NewDurableRunObserver("blocked")
		o.Observe(Event{Type: "text", Data: "bounded partial answer"})
		o.Observe(Event{Type: "done", Data: map[string]any{
			"terminalState":       EvidenceTerminalVerified,
			"completionStatus":    CompletionStatusBlocked,
			"completionRevision":  2,
			"completionCriteria":  3,
			"completionSatisfied": 1,
			"completionBlocked":   1,
		}})
		metadata, ok := o.TerminalMetadata()
		if !ok || !o.Completed() || len(o.Messages()) != 1 {
			t.Fatalf("blocked done was not stream-committable: ok=%v completed=%v messages=%#v", ok, o.Completed(), o.Messages())
		}
		if metadata.TerminalState != EvidenceTerminalBlocked || metadata.CompletionStatus != CompletionStatusBlocked {
			t.Fatalf("blocked completion was reinterpreted as success: %#v", metadata)
		}
		if !metadata.BlocksSuccess() {
			t.Fatalf("blocked completion allowed success: %#v", metadata)
		}
	})

	t.Run("blocked count fails closed against inconsistent status", func(t *testing.T) {
		o := NewDurableRunObserver("inconsistent")
		o.Observe(Event{Type: "done", Data: map[string]any{
			"terminalState":      EvidenceTerminalVerified,
			"completionStatus":   CompletionStatusComplete,
			"completionCriteria": 2,
			"completionBlocked":  1,
		}})
		metadata, ok := o.TerminalMetadata()
		if !ok || metadata.TerminalState != EvidenceTerminalBlocked || metadata.CompletionStatus != CompletionStatusBlocked {
			t.Fatalf("inconsistent blocked metadata did not fail closed: %#v ok=%v", metadata, ok)
		}
		decoded, decodedOK := DecodeDurableRunTerminalMetadata(map[string]any{
			"completionStatus":  CompletionStatusIncomplete,
			"completionBlocked": 0,
		})
		if !decodedOK || !decoded.BlocksSuccess() || decoded.TerminalState != EvidenceTerminalBlocked {
			t.Fatalf("exported terminal decoder did not fail closed: %#v ok=%v", decoded, decodedOK)
		}
	})
}
