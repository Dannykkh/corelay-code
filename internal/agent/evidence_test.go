package agent

import (
	"encoding/json"
	"testing"
)

func TestClassifyEvidenceMode(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"quick explanation", "간단히 방향만 확인하자", EvidenceModeQuick},
		{"normal edit", "fix the settings label", EvidenceModeNormal},
		{"deep completion", "크로노스. 완료까지 진행하자.", EvidenceModeDeep},
		{"risk escalates", "update auth token validation", EvidenceModeDeep},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := ClassifyEvidenceMode(tt.in)
			if got != tt.want {
				t.Fatalf("ClassifyEvidenceMode(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEvidenceGateDeepChangeMeasureWouldBlock(t *testing.T) {
	ledger := NewEvidenceLedger("완료까지 구현하자", EvidencePolicyConfig{Policy: EvidencePolicyMeasure})
	ledger.ObserveChangedFile("proxy-go/internal/agent/loop.go")

	got := ledger.Evaluate()
	if got.Decision != EvidenceGateWouldBlock || got.TerminalState != EvidenceTerminalUnverified || got.Mode != EvidenceModeDeep {
		t.Fatalf("Evaluate() = %+v, want would-block/unverified deep", got)
	}
}

func TestEvidenceGateBlockStopsUntilEvidence(t *testing.T) {
	ledger := NewEvidenceLedger("finish production-ready implementation", EvidencePolicyConfig{
		Policy:        EvidencePolicyBlock,
		MaxStopBlocks: 1,
	})
	ledger.ObserveChangedFile("web/app/page.tsx")

	first := ledger.Evaluate()
	if first.Decision != EvidenceGateBlock || first.TerminalState != EvidenceTerminalBlocked {
		t.Fatalf("first Evaluate() = %+v, want block/blocked", first)
	}
	ledger.MarkStopBlock()
	second := ledger.Evaluate()
	if second.Decision != EvidenceGateWarn || second.TerminalState != EvidenceTerminalUnverified {
		t.Fatalf("second Evaluate() = %+v, want warn/unverified after stop budget", second)
	}
}

func TestEvidenceGateAllowsDocsOnly(t *testing.T) {
	ledger := NewEvidenceLedger("전체 사용 설명서를 완료하자", EvidencePolicyConfig{Policy: EvidencePolicyBlock})
	ledger.ObserveChangedFile("docs/manual.md")

	got := ledger.Evaluate()
	if got.Decision != EvidenceGateAllow {
		t.Fatalf("Evaluate() = %+v, want allow for docs-only", got)
	}
}

func TestEvidenceObservesVerificationCommand(t *testing.T) {
	ledger := NewEvidenceLedger("complete implementation", EvidencePolicyConfig{Policy: EvidencePolicyBlock})
	ledger.ObserveChangedFile("internal/service.go")
	input, _ := json.Marshal(map[string]string{"command": "go test ./internal/agent"})
	ledger.ObserveToolResult("Bash", input, "ok", false)

	got := ledger.Evaluate()
	if got.Decision != EvidenceGateAllow || got.TerminalState != EvidenceTerminalVerified {
		t.Fatalf("Evaluate() = %+v, want allow/verified after verification", got)
	}
	receipt := ledger.ApplyToReceipt(ReceiptVerification{Status: "passed", Source: "auto-verify"})
	if receipt.Gate != EvidenceGateAllow || receipt.TerminalState != EvidenceTerminalVerified || receipt.Mode != EvidenceModeDeep || len(receipt.Evidence) != 1 {
		t.Fatalf("receipt evidence not applied: %+v", receipt)
	}
}

func TestEvidenceTerminalStateTable(t *testing.T) {
	tests := []struct {
		name     string
		decision string
		status   string
		records  []EvidenceRecord
		want     string
	}{
		{name: "policy block", decision: EvidenceGateBlock, status: "passed", want: EvidenceTerminalBlocked},
		{name: "clean pass", decision: EvidenceGateAllow, status: "passed", want: EvidenceTerminalVerified},
		{name: "passed receipt without gate", status: "passed", want: EvidenceTerminalVerified},
		{name: "mixed evidence", decision: EvidenceGateAllow, records: []EvidenceRecord{{Status: "failed"}, {Status: "passed"}}, want: EvidenceTerminalPartiallyVerified},
		{name: "advisory pass", decision: EvidenceGateWarn, status: "passed", want: EvidenceTerminalPartiallyVerified},
		{name: "failed only", decision: EvidenceGateAllow, status: "failed", want: EvidenceTerminalUnverified},
		{name: "no evidence", decision: EvidenceGateAllow, status: "not-run", want: EvidenceTerminalUnverified},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evidenceTerminalState(tt.decision, tt.status, tt.records); got != tt.want {
				t.Fatalf("evidenceTerminalState(%q, %q, %#v) = %q, want %q", tt.decision, tt.status, tt.records, got, tt.want)
			}
		})
	}
}
