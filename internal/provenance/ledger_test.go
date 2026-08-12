package provenance_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

type sourceRecord struct {
	Project    string `json:"project"`
	URL        string `json:"url"`
	Commit     string `json:"commit"`
	ObservedAt string `json:"observedAt"`
}

type capabilityRecord struct {
	CapabilityID         string   `json:"capabilityId"`
	Status               string   `json:"status"`
	SourceIDs            []string `json:"sourceIds"`
	BehaviorContract     string   `json:"behaviorContract"`
	NonCopiedConstraints []string `json:"nonCopiedConstraints"`
	Owner                string   `json:"owner"`
	ImplementationFiles  []string `json:"implementationFiles"`
	TestFiles            []string `json:"testFiles"`
	Reviewer             string   `json:"reviewer"`
	VerifiedAt           string   `json:"verifiedAt"`
}

type ledger struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Scope         string                  `json:"scope"`
	Sources       map[string]sourceRecord `json:"sources"`
	Records       []capabilityRecord      `json:"records"`
}

var (
	capabilityIDPattern = regexp.MustCompile(`^CAP-[0-9]{3}$`)
	commitPattern       = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func TestCapabilityProvenanceLedger(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	path := filepath.Join(root, "docs", "plan", "agent-capability-absorption", "provenance-ledger.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read provenance ledger: %v", err)
	}
	var got ledger
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("decode provenance ledger: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("provenance ledger must contain exactly one JSON value: %v", err)
	}
	if got.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", got.SchemaVersion)
	}
	if strings.TrimSpace(got.Scope) == "" {
		t.Fatal("scope is required")
	}
	validateSources(t, root, got.Sources)
	matrixStatuses := readCapabilityMatrixStatuses(t, root)

	seen := make(map[string]struct{}, len(got.Records))
	recordStatuses := make(map[string]string, len(got.Records))
	for index, record := range got.Records {
		label := fmt.Sprintf("records[%d]", index)
		if !capabilityIDPattern.MatchString(record.CapabilityID) {
			t.Errorf("%s capabilityId %q is invalid", label, record.CapabilityID)
		}
		if _, duplicate := seen[record.CapabilityID]; duplicate {
			t.Errorf("duplicate capabilityId %q", record.CapabilityID)
		}
		seen[record.CapabilityID] = struct{}{}
		recordStatuses[record.CapabilityID] = record.Status
		if record.Status != "connected" && record.Status != "verified" {
			t.Errorf("%s status %q must be connected or verified", label, record.Status)
		}
		if len(record.SourceIDs) == 0 || strings.TrimSpace(record.BehaviorContract) == "" || strings.TrimSpace(record.Owner) == "" {
			t.Errorf("%s is missing sourceIds, behaviorContract, or owner", label)
		}
		if len(record.NonCopiedConstraints) < 2 {
			t.Errorf("%s must state at least two non-copied constraints", label)
		}
		for _, sourceID := range record.SourceIDs {
			if _, ok := got.Sources[sourceID]; !ok {
				t.Errorf("%s references unknown source %q", label, sourceID)
			}
		}
		validatePaths(t, root, label+" implementationFiles", record.ImplementationFiles)
		validatePaths(t, root, label+" testFiles", record.TestFiles)
		if record.Status == "verified" {
			if strings.TrimSpace(record.Reviewer) == "" {
				t.Errorf("%s verified record has no reviewer", label)
			}
			if _, err := time.Parse("2006-01-02", record.VerifiedAt); err != nil {
				t.Errorf("%s verifiedAt %q is invalid: %v", label, record.VerifiedAt, err)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("ledger has no capability records")
	}
	validateMatrixLedgerAlignment(t, matrixStatuses, recordStatuses)
	validateClaimedAuditCoverage(t, matrixStatuses, recordStatuses)
}

func readCapabilityMatrixStatuses(t *testing.T, root string) map[string]string {
	t.Helper()
	path := filepath.Join(root, "docs", "plan", "agent-capability-absorption", "capability-matrix.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capability matrix: %v", err)
	}
	statuses := make(map[string]string, 45)
	allowed := map[string]struct{}{
		"Missing": {}, "Partial": {}, "Connected": {}, "Verified": {},
	}
	for lineNumber, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "| CAP-") {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 5 {
			t.Errorf("capability matrix line %d is malformed", lineNumber+1)
			continue
		}
		id := strings.TrimSpace(fields[1])
		status := strings.TrimSpace(fields[3])
		if !capabilityIDPattern.MatchString(id) {
			t.Errorf("capability matrix line %d has invalid ID %q", lineNumber+1, id)
			continue
		}
		if _, duplicate := statuses[id]; duplicate {
			t.Errorf("capability matrix has duplicate row %q", id)
		}
		if _, ok := allowed[status]; !ok {
			t.Errorf("capability matrix %s status %q must be Missing, Partial, Connected, or Verified", id, status)
		}
		statuses[id] = status
	}
	for number := 1; number <= 45; number++ {
		id := fmt.Sprintf("CAP-%03d", number)
		if _, ok := statuses[id]; !ok {
			t.Errorf("capability matrix is missing %s", id)
		}
	}
	if len(statuses) != 45 {
		t.Errorf("capability matrix has %d capability rows, want 45", len(statuses))
	}
	return statuses
}

func validateMatrixLedgerAlignment(t *testing.T, matrix, records map[string]string) {
	t.Helper()
	for id, matrixStatus := range matrix {
		wantRecordStatus := ""
		switch matrixStatus {
		case "Connected":
			wantRecordStatus = "connected"
		case "Verified":
			wantRecordStatus = "verified"
		}
		gotRecordStatus, recorded := records[id]
		if wantRecordStatus == "" {
			if recorded {
				t.Errorf("%s is %s in matrix but has %q ledger record", id, matrixStatus, gotRecordStatus)
			}
			continue
		}
		if !recorded {
			t.Errorf("%s is %s in matrix but has no provenance ledger record", id, matrixStatus)
			continue
		}
		if gotRecordStatus != wantRecordStatus {
			t.Errorf("%s status mismatch: matrix=%s ledger=%s", id, matrixStatus, gotRecordStatus)
		}
	}
	for id, recordStatus := range records {
		if _, ok := matrix[id]; !ok {
			t.Errorf("ledger record %s=%s has no capability matrix row", id, recordStatus)
		}
	}
}

func validateClaimedAuditCoverage(t *testing.T, matrix, records map[string]string) {
	t.Helper()
	if _, claimed := records["CAP-045"]; !claimed {
		return
	}
	if len(records) != len(matrix) {
		t.Errorf("CAP-045 claims full audit coverage but ledger has %d records for %d matrix rows", len(records), len(matrix))
	}
	for id := range matrix {
		if _, ok := records[id]; !ok {
			t.Errorf("CAP-045 claims full audit coverage but ledger is missing %s", id)
		}
	}
}

func validateSources(t *testing.T, root string, sources map[string]sourceRecord) {
	t.Helper()
	for id, source := range sources {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(source.Project) == "" || strings.TrimSpace(source.URL) == "" {
			t.Errorf("source %q is incomplete", id)
			continue
		}
		if _, err := time.Parse("2006-01-02", source.ObservedAt); err != nil {
			t.Errorf("source %q observedAt %q is invalid: %v", id, source.ObservedAt, err)
		}
		parsed, err := url.Parse(source.URL)
		if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
			if parsed.Host == "github.com" {
				if !strings.Contains(parsed.Path, "/blob/") || strings.Contains(parsed.Path, "/blob/main/") {
					t.Errorf("GitHub source %q must be pinned to a commit: %s", id, source.URL)
				}
				if !commitPattern.MatchString(source.Commit) || !strings.Contains(parsed.Path, source.Commit) {
					t.Errorf("GitHub source %q commit does not match its URL", id)
				}
			}
			continue
		}
		validatePaths(t, root, "source "+id, []string{source.URL})
	}
}

func validatePaths(t *testing.T, root, label string, paths []string) {
	t.Helper()
	if len(paths) == 0 {
		t.Errorf("%s is empty", label)
		return
	}
	for _, path := range paths {
		if filepath.IsAbs(path) || strings.TrimSpace(path) == "" {
			t.Errorf("%s contains non-relative path %q", label, path)
			continue
		}
		full := filepath.Clean(filepath.Join(root, filepath.FromSlash(path)))
		rel, err := filepath.Rel(root, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("%s path escapes repository: %q", label, path)
			continue
		}
		info, err := os.Stat(full)
		if err != nil {
			t.Errorf("%s path %q does not exist: %v", label, path, err)
			continue
		}
		if !info.Mode().IsRegular() {
			t.Errorf("%s path %q is not a regular file", label, path)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve provenance test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
