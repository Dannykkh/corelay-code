package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aniclew/aniclew/internal/config"
)

func TestEvidencePolicyHandlers(t *testing.T) {
	t.Setenv("ANICLEW_CONFIG_DIR", t.TempDir())
	t.Setenv("ANICLEW_EVIDENCE_POLICY", "advisory")
	s := New(nil, "", 0)

	getReq := httptest.NewRequest(http.MethodGet, "/api/evidence/policy", nil)
	getRec := httptest.NewRecorder()
	s.handleEvidencePolicy(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var got evidencePolicyResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if got.Policy != "advisory" || got.MaxStopBlocks != 2 {
		t.Fatalf("policy GET = %+v", got)
	}

	putBody := bytes.NewBufferString(`{"policy":"block","maxStopBlocks":3}`)
	putReq := httptest.NewRequest(http.MethodPut, "/api/evidence/policy", putBody)
	putRec := httptest.NewRecorder()
	s.handleEvidencePolicyUpdate(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	if err := json.Unmarshal(putRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode PUT: %v", err)
	}
	if got.Policy != "block" || got.MaxStopBlocks != 3 {
		t.Fatalf("policy PUT = %+v", got)
	}
	saved := config.Load()
	if saved.EvidencePolicy != "block" || saved.EvidenceMaxStopBlocks != 3 {
		t.Fatalf("saved evidence policy = %q/%d, want block/3", saved.EvidencePolicy, saved.EvidenceMaxStopBlocks)
	}
}

func TestReadRecentEvidenceReceipts(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	otherWorkspace := filepath.Join(base, "other")
	receiptDir := filepath.Join(base, "receipts", "workspace")
	if err := os.MkdirAll(receiptDir, 0755); err != nil {
		t.Fatal(err)
	}

	older := time.Date(2026, 7, 4, 1, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	writeReceiptFile(t, filepath.Join(receiptDir, "agent.json"), map[string]any{
		"version":   1,
		"createdAt": older.Format(time.RFC3339Nano),
		"workDir":   workspace,
		"provider":  "fake",
		"model":     "fake-model",
		"editedFiles": []string{
			"internal/agent/evidence.go",
		},
		"verification": map[string]any{
			"status": "passed",
			"source": "auto-verify",
			"gate":   "allow",
			"mode":   "deep",
		},
	})
	writeReceiptFile(t, filepath.Join(receiptDir, "team.json"), map[string]any{
		"version":       1,
		"kind":          "team-run",
		"createdAt":     newer.Format(time.RFC3339Nano),
		"workDir":       workspace,
		"provider":      "fake",
		"model":         "fake-model",
		"verifyCommand": "go test ./...",
		"verification": map[string]any{
			"status":  "failed",
			"source":  "team-verify",
			"summary": "tests failed",
		},
	})
	writeReceiptFile(t, filepath.Join(receiptDir, "other.json"), map[string]any{
		"version":   1,
		"createdAt": newer.Add(time.Hour).Format(time.RFC3339Nano),
		"workDir":   otherWorkspace,
		"provider":  "fake",
		"model":     "fake-model",
		"verification": map[string]any{
			"status": "passed",
			"source": "auto-verify",
		},
	})

	items := readRecentEvidenceReceipts(filepath.Join(base, "receipts"), 10)
	if len(items) != 3 {
		t.Fatalf("items=%d, want 3", len(items))
	}
	if items[0].WorkDir != otherWorkspace {
		t.Fatalf("global newest item not sorted first: %+v", items[0])
	}

	currentItems := readRecentEvidenceReceiptsForWorkspace(filepath.Join(base, "receipts"), 10, workspace)
	if len(currentItems) != 2 {
		t.Fatalf("current workspace items=%d, want 2", len(currentItems))
	}
	if currentItems[0].Kind != "team-run" || currentItems[0].Command != "go test ./..." || currentItems[0].Status != "failed" {
		t.Fatalf("newest current item not parsed/sorted: %+v", currentItems[0])
	}
	if currentItems[0].Summary != "tests failed" || currentItems[0].Workspace != "workspace" {
		t.Fatalf("team summary/workspace not parsed: %+v", currentItems[0])
	}
	if currentItems[1].Kind != "agent" || currentItems[1].Gate != "allow" || len(currentItems[1].EditedFiles) != 1 {
		t.Fatalf("agent item not parsed: %+v", currentItems[1])
	}
	if currentItems[1].Summary != "1 edited file(s), no successful verification evidence" ||
		currentItems[1].EditedFileCount != 1 {
		t.Fatalf("agent synthesized summary/count wrong: %+v", currentItems[1])
	}
}

func writeReceiptFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}
