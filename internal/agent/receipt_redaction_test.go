package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSanitizeReceiptStringSecretCorpus(t *testing.T) {
	privateKey := "-----BEGIN OPENSSH PRIVATE KEY-----\nvery-secret-key-material\n-----END OPENSSH PRIVATE KEY-----"
	tests := []struct {
		name   string
		input  string
		secret string
	}{
		{"authorization bearer", "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.secret", "eyJhbGciOiJIUzI1NiJ9.secret"},
		{"authorization basic", "Authorization=Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA=="},
		{"json authorization basic", `{"Authorization":"Basic dXNlcjpwYXNzd29yZA=="}`, "dXNlcjpwYXNzd29yZA=="},
		{"standalone bearer", "use Bearer abcdefghijklmnop", "abcdefghijklmnop"},
		{"json password", `{"password":"correct horse battery staple"}`, "correct horse battery staple"},
		{"env api key", "OPENAI_API_KEY=top-secret-api-value", "top-secret-api-value"},
		{"env token", "GITHUB_TOKEN='top-secret-token-value'", "top-secret-token-value"},
		{"url credentials", "https://build-user:build-password@example.test/repo", "build-password"},
		{"openai key", "sk-1234567890abcdefghijklmnop", "sk-1234567890abcdefghijklmnop"},
		{"github key", "ghp_1234567890abcdefghijklmnop", "ghp_1234567890abcdefghijklmnop"},
		{"aws access key", "AKIA1234567890ABCDEF", "AKIA1234567890ABCDEF"},
		{"slack token", "xoxb-1234567890-abcdefghijkl", "xoxb-1234567890-abcdefghijkl"},
		{"private key", privateKey, "very-secret-key-material"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeReceiptString(tt.input)
			if strings.Contains(got, tt.secret) {
				t.Fatalf("secret remained in sanitized value: %q", got)
			}
			if !strings.Contains(got, "REDACTED") {
				t.Fatalf("sanitized value lacks redaction marker: %q", got)
			}
		})
	}
}

func TestSanitizeReceiptStringAvoidsBroadFalsePositives(t *testing.T) {
	values := []string{
		"The token budget is 4096.",
		"tokenizer=sentencepiece",
		"password policy requires twelve characters",
		"sk-demo-placeholder",
		"https://example.test/path@revision",
	}
	for _, value := range values {
		if got := sanitizeReceiptString(value); got != value {
			t.Fatalf("sanitizeReceiptString(%q) = %q", value, got)
		}
	}

	oversized := strings.Repeat("x", maxReceiptStringBytes+1)
	if got := sanitizeReceiptString(oversized); got != "[REDACTED: oversized receipt field]" {
		t.Fatalf("oversized corpus was not bounded: %q", got)
	}
}

func TestMarshalSanitizedTeamReceiptNestedAndNoInputMutation(t *testing.T) {
	workDir := t.TempDir()
	artifactPath := filepath.Join(workDir, "result.txt")
	if err := os.WriteFile(artifactPath, []byte("artifact contents"), 0600); err != nil {
		t.Fatal(err)
	}
	command := "curl -H 'Authorization: Bearer abcdefghijklmnop' https://example.test"
	evidenceCommand := "OPENAI_API_KEY=top-secret-api-value go test ./..."
	receipt := TeamRunReceipt{
		Version:       1,
		Kind:          "team-run",
		CreatedAt:     "2026-08-12T00:00:00Z",
		WorkDir:       workDir,
		Status:        "completed",
		TeamName:      "team password=team-secret-value",
		Objective:     "Ship safely with GITHUB_TOKEN=top-secret-token-value",
		Provider:      "provider",
		Model:         "model",
		VerifyCommand: command,
		Verification: ReceiptVerification{
			Status:  "passed",
			Source:  "tool",
			Command: command,
			Summary: `{"password":"summary-secret-value"}`,
			Evidence: []EvidenceRecord{{
				Source:  "tool",
				Command: evidenceCommand,
				Status:  "passed",
				Summary: "Authorization: Basic dXNlcjpwYXNzd29yZA==",
			}},
		},
		Tasks: []TeamTaskReceipt{{
			ID:            "task-1",
			Name:          "task",
			Description:   "password=description-secret-value",
			Status:        "completed",
			Files:         []string{"result.txt", "internal/agent/**"},
			OutputPath:    "https://user:path-secret@example.test/output",
			ResultSummary: "Bearer result-summary-secret-value",
		}},
	}
	before := deepCopyTeamReceiptForTest(receipt)

	data, err := marshalSanitizedReceipt(workDir, receipt)
	if err != nil {
		t.Fatalf("marshalSanitizedReceipt() error = %v", err)
	}
	if !reflect.DeepEqual(receipt, before) {
		t.Fatalf("marshal boundary mutated input\nbefore: %#v\nafter:  %#v", before, receipt)
	}
	genericJSON, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("json.Marshal(TeamRunReceipt) error = %v", err)
	}
	if strings.Contains(string(genericJSON), "top-secret-token-value") {
		t.Fatal("TeamRunReceipt.MarshalJSON bypassed the sanitizing boundary")
	}
	for _, secret := range []string{
		"team-secret-value", "top-secret-token-value", "abcdefghijklmnop",
		"top-secret-api-value", "summary-secret-value", "dXNlcjpwYXNzd29yZA==",
		"description-secret-value", "path-secret", "result-summary-secret-value",
	} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("serialized receipt contains secret %q: %s", secret, data)
		}
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if got := payload["verifyCommandDigest"]; got != sha256String(command) {
		t.Fatalf("verifyCommandDigest = %v", got)
	}
	verification := payload["verification"].(map[string]any)
	if got := verification["commandDigest"]; got != sha256String(command) {
		t.Fatalf("verification commandDigest = %v", got)
	}
	evidence := verification["evidence"].([]any)[0].(map[string]any)
	if got := evidence["commandDigest"]; got != sha256String(evidenceCommand) {
		t.Fatalf("evidence commandDigest = %v", got)
	}
	if strings.Contains(evidence["command"].(string), "top-secret-api-value") {
		t.Fatalf("evidence command was not redacted: %v", evidence["command"])
	}
	task := payload["tasks"].([]any)[0].(map[string]any)
	artifacts := task["artifacts"].([]any)
	if len(artifacts) != 2 || artifacts[0].(map[string]any)["status"] != string(ReceiptArtifactOK) || artifacts[1].(map[string]any)["status"] != string(ReceiptArtifactPattern) {
		t.Fatalf("team task artifacts = %#v", artifacts)
	}
}

func TestReceiptArtifactMetadataAndBoundaryStatuses(t *testing.T) {
	workDir := t.TempDir()
	baseDir := t.TempDir()
	content := []byte("content that must never be stored in the receipt")
	if err := os.MkdirAll(filepath.Join(workDir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "nested", "file.txt"), content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workDir, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outsidePath, []byte("outside secret contents"), 0600); err != nil {
		t.Fatal(err)
	}

	paths := []string{"nested/file.txt", "missing.txt", "directory", outsidePath}
	symlinkCreated := false
	symlinkPath := filepath.Join(workDir, "escape-link")
	if err := os.Symlink(outsidePath, symlinkPath); err == nil {
		paths = append(paths, "escape-link")
		symlinkCreated = true
	}

	written, err := writeAgentReceiptToDir(baseDir, workDir, AgentReceipt{
		EditedFiles: paths,
		Verification: ReceiptVerification{
			Status:  "passed",
			Source:  "tool",
			Command: "go test ./...",
		},
	}, time.Date(2026, 8, 12, 1, 2, 3, 4, time.UTC))
	if err != nil {
		t.Fatalf("writeAgentReceiptToDir() error = %v", err)
	}
	data, err := os.ReadFile(written)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), string(content)) || strings.Contains(string(data), "outside secret contents") {
		t.Fatal("receipt stored artifact contents")
	}
	var receipt AgentReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]ReceiptArtifact)
	for _, artifact := range receipt.Artifacts {
		statuses[artifact.Path] = artifact
	}
	regular := statuses["nested/file.txt"]
	if regular.Status != ReceiptArtifactOK || regular.Size == nil || *regular.Size != int64(len(content)) {
		t.Fatalf("regular artifact = %+v", regular)
	}
	sum := sha256.Sum256(content)
	if regular.SHA256 != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("artifact digest = %q", regular.SHA256)
	}
	if statuses["missing.txt"].Status != ReceiptArtifactMissing {
		t.Fatalf("missing artifact = %+v", statuses["missing.txt"])
	}
	if statuses["directory"].Status != ReceiptArtifactNonRegular {
		t.Fatalf("directory artifact = %+v", statuses["directory"])
	}
	if statuses["[outside-workspace]"].Status != ReceiptArtifactOutsideWorkspace {
		t.Fatalf("outside artifact = %+v", statuses["[outside-workspace]"])
	}
	if symlinkCreated && statuses["escape-link"].Status != ReceiptArtifactSymlink {
		t.Fatalf("symlink artifact = %+v", statuses["escape-link"])
	}
	if receipt.Verification.CommandDigest != sha256String("go test ./...") {
		t.Fatalf("verification digest = %q", receipt.Verification.CommandDigest)
	}
}

func TestReceiptWorkspaceDigestAndAtomicUniqueWrites(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	workDir, err := os.MkdirTemp(cwd, "receipt-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })
	relative, err := filepath.Rel(cwd, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if safeReceiptDir(relative) != safeReceiptDir(workDir) {
		t.Fatalf("relative and absolute workspace keys differ: %s != %s", safeReceiptDir(relative), safeReceiptDir(workDir))
	}
	if safeReceiptDir(filepath.Join("D:", "a", "b")) == safeReceiptDir(filepath.Join("D:", "a--b")) {
		t.Fatal("lossy workspace names collided")
	}
	if runtime.GOOS == "windows" && safeReceiptDir(strings.ToUpper(workDir)) != safeReceiptDir(strings.ToLower(workDir)) {
		t.Fatal("Windows workspace key is case-sensitive")
	}

	baseDir := t.TempDir()
	now := time.Date(2026, 8, 12, 2, 3, 4, 5, time.UTC)
	first, err := writeAgentReceiptToDir(baseDir, workDir, AgentReceipt{}, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := writeAgentReceiptToDir(baseDir, workDir, AgentReceipt{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("same-timestamp receipts collided")
	}
	for _, path := range []string{first, second} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("receipt mode = %o, want 600", info.Mode().Perm())
		}
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(first), ".receipt-*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary receipt files remain: %v, err=%v", temps, err)
	}
}

func TestReceiptAtomicFailureLeavesNoReceipt(t *testing.T) {
	basePath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(basePath, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeAgentReceiptToDir(basePath, t.TempDir(), AgentReceipt{}, time.Now()); err == nil {
		t.Fatal("writeAgentReceiptToDir() error = nil, want base-directory failure")
	}
	data, err := os.ReadFile(basePath)
	if err != nil || string(data) != "occupied" {
		t.Fatalf("failure changed existing target: %q, err=%v", data, err)
	}
}

func deepCopyTeamReceiptForTest(receipt TeamRunReceipt) TeamRunReceipt {
	copyReceipt := receipt
	copyReceipt.Tasks = append([]TeamTaskReceipt(nil), receipt.Tasks...)
	for i := range copyReceipt.Tasks {
		copyReceipt.Tasks[i].Files = append([]string(nil), receipt.Tasks[i].Files...)
		copyReceipt.Tasks[i].DependsOn = append([]string(nil), receipt.Tasks[i].DependsOn...)
	}
	copyReceipt.Verification.Evidence = append([]EvidenceRecord(nil), receipt.Verification.Evidence...)
	return copyReceipt
}
