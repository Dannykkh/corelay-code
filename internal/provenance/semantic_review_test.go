package provenance_test

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	maximumSemanticReviewBytes      = 256 << 10
	maximumSemanticEvidenceBytes    = 2 << 10
	maximumSemanticLimitations      = 8
	maximumSemanticLimitationBytes  = 2 << 10
	expectedSemanticCapabilityCount = 45
)

type semanticReviewer struct {
	ID                    string `json:"id"`
	Role                  string `json:"role"`
	TaskPath              string `json:"taskPath"`
	IndependenceStatement string `json:"independenceStatement"`
	PriorAccessDisclosure string `json:"priorAccessDisclosure"`
}

type semanticCriterion struct {
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence"`
}

type semanticCriteria struct {
	Contract     semanticCriterion `json:"contract"`
	HappyPath    semanticCriterion `json:"happyPath"`
	NegativePath semanticCriterion `json:"negativePath"`
	Composition  semanticCriterion `json:"composition"`
	Provenance   semanticCriterion `json:"provenance"`
}

type semanticReviewRecord struct {
	CapabilityID                string            `json:"capabilityId"`
	ReviewerID                  string            `json:"reviewerId"`
	LedgerStatusAtReview        string            `json:"ledgerStatusAtReview"`
	ReviewedImplementationPaths []string          `json:"reviewedImplementationPaths"`
	ReviewedTestPaths           []string          `json:"reviewedTestPaths"`
	Criteria                    semanticCriteria  `json:"criteria"`
	ResidualLimitations         []string          `json:"residualLimitations"`
	ReviewFindings              []semanticFinding `json:"reviewFindings,omitempty"`
	OverallVerdict              string            `json:"overallVerdict"`
}

type semanticFinding struct {
	ID                 string `json:"id"`
	ObservedAt         string `json:"observedAt"`
	Severity           string `json:"severity"`
	Criterion          string `json:"criterion"`
	Status             string `json:"status"`
	Evidence           string `json:"evidence"`
	ResolutionEvidence string `json:"resolutionEvidence,omitempty"`
}

type semanticReviewSummary struct {
	RecordCount                    int `json:"recordCount"`
	Pass                           int `json:"pass"`
	PassWithLimitations            int `json:"passWithLimitations"`
	Fail                           int `json:"fail"`
	VerifiedLedgerRecordsReviewed  int `json:"verifiedLedgerRecordsReviewed"`
	ConnectedLedgerRecordsReviewed int `json:"connectedLedgerRecordsReviewed"`
}

type semanticReviewArtifact struct {
	SchemaVersion int                    `json:"schemaVersion"`
	ReviewID      string                 `json:"reviewId"`
	ReviewedAt    string                 `json:"reviewedAt"`
	SourceMatrix  string                 `json:"sourceMatrix"`
	SourceLedger  string                 `json:"sourceLedger"`
	Reviewer      semanticReviewer       `json:"reviewer"`
	Scope         string                 `json:"scope"`
	Records       []semanticReviewRecord `json:"records"`
	Summary       semanticReviewSummary  `json:"summary"`
}

func TestCapabilitySemanticReview(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	reviewPath := filepath.Join(root, "docs", "plan", "agent-capability-absorption", "semantic-review.json")
	data, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatalf("read semantic review: %v", err)
	}
	if len(data) == 0 || len(data) > maximumSemanticReviewBytes {
		t.Fatalf("semantic review size = %d, want 1..%d", len(data), maximumSemanticReviewBytes)
	}

	var review semanticReviewArtifact
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&review); err != nil {
		t.Fatalf("decode semantic review: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("semantic review must contain exactly one JSON value: %v", err)
	}
	if review.SchemaVersion != 1 {
		t.Fatalf("schemaVersion = %d, want 1", review.SchemaVersion)
	}
	if strings.TrimSpace(review.ReviewID) == "" || len(review.ReviewID) > 128 {
		t.Fatalf("reviewId %q is invalid", review.ReviewID)
	}
	if _, err := time.Parse("2006-01-02", review.ReviewedAt); err != nil {
		t.Fatalf("reviewedAt %q is invalid: %v", review.ReviewedAt, err)
	}
	if strings.TrimSpace(review.Scope) == "" || len(review.Scope) > maximumSemanticEvidenceBytes {
		t.Fatalf("semantic review scope is empty or oversized")
	}
	validatePaths(t, root, "semantic review sourceMatrix", []string{review.SourceMatrix})
	validatePaths(t, root, "semantic review sourceLedger", []string{review.SourceLedger})
	validateIndependentReviewer(t, review.Reviewer)

	ledgerRecords := readSemanticLedger(t, root, review.SourceLedger)
	matrixStatuses := readCapabilityMatrixStatuses(t, root)
	if len(review.Records) != expectedSemanticCapabilityCount {
		t.Fatalf("semantic review records = %d, want %d", len(review.Records), expectedSemanticCapabilityCount)
	}

	seen := make(map[string]struct{}, expectedSemanticCapabilityCount)
	computed := semanticReviewSummary{RecordCount: len(review.Records)}
	for index, record := range review.Records {
		label := fmt.Sprintf("records[%d]", index)
		if !capabilityIDPattern.MatchString(record.CapabilityID) {
			t.Errorf("%s capabilityId %q is invalid", label, record.CapabilityID)
			continue
		}
		if _, duplicate := seen[record.CapabilityID]; duplicate {
			t.Errorf("duplicate semantic capabilityId %q", record.CapabilityID)
		}
		seen[record.CapabilityID] = struct{}{}
		if record.ReviewerID != review.Reviewer.ID {
			t.Errorf("%s reviewerId %q does not match independent reviewer %q", label, record.ReviewerID, review.Reviewer.ID)
		}

		provenance, ok := ledgerRecords[record.CapabilityID]
		if !ok {
			t.Errorf("%s has no provenance ledger record", record.CapabilityID)
			continue
		}
		if record.LedgerStatusAtReview != provenance.Status {
			t.Errorf("%s ledgerStatusAtReview=%q, ledger=%q", record.CapabilityID, record.LedgerStatusAtReview, provenance.Status)
		}
		if want := matrixStatusForLedger(provenance.Status); matrixStatuses[record.CapabilityID] != want {
			t.Errorf("%s matrix=%q, ledger=%q", record.CapabilityID, matrixStatuses[record.CapabilityID], provenance.Status)
		}
		if provenance.Status == "verified" {
			computed.VerifiedLedgerRecordsReviewed++
		} else if provenance.Status == "connected" {
			computed.ConnectedLedgerRecordsReviewed++
		}

		validateUniquePaths(t, root, label+" reviewedImplementationPaths", record.ReviewedImplementationPaths)
		validateUniquePaths(t, root, label+" reviewedTestPaths", record.ReviewedTestPaths)
		requireReviewedPaths(t, record.CapabilityID+" implementation", provenance.ImplementationFiles, record.ReviewedImplementationPaths)
		requireReviewedPaths(t, record.CapabilityID+" tests", provenance.TestFiles, record.ReviewedTestPaths)

		criteria := []struct {
			name  string
			value semanticCriterion
		}{
			{"contract", record.Criteria.Contract},
			{"happyPath", record.Criteria.HappyPath},
			{"negativePath", record.Criteria.NegativePath},
			{"composition", record.Criteria.Composition},
			{"provenance", record.Criteria.Provenance},
		}
		partial, failed := false, false
		for _, criterion := range criteria {
			validateSemanticCriterion(t, record.CapabilityID+" "+criterion.name, criterion.value)
			partial = partial || criterion.value.Verdict == "partial"
			failed = failed || criterion.value.Verdict == "fail"
		}
		validateSemanticLimitations(t, record.CapabilityID, record.ResidualLimitations)
		validateSemanticFindings(t, record)
		switch record.OverallVerdict {
		case "pass":
			computed.Pass++
			if partial || failed {
				t.Errorf("%s overall pass contains partial or failed criterion", record.CapabilityID)
			}
		case "pass-with-limitations":
			computed.PassWithLimitations++
			if failed || (!partial && len(record.ResidualLimitations) == 0) {
				t.Errorf("%s pass-with-limitations lacks a partial criterion/limitation or contains a failure", record.CapabilityID)
			}
		case "fail":
			computed.Fail++
			if !failed {
				t.Errorf("%s overall fail has no failed criterion", record.CapabilityID)
			}
		default:
			t.Errorf("%s overallVerdict %q is invalid", record.CapabilityID, record.OverallVerdict)
		}
		validateVerdictStatusCompatibility(t, record, provenance.Status, partial, failed)
	}

	for number := 1; number <= expectedSemanticCapabilityCount; number++ {
		id := fmt.Sprintf("CAP-%03d", number)
		if _, ok := seen[id]; !ok {
			t.Errorf("semantic review is missing %s", id)
		}
	}
	if len(seen) != expectedSemanticCapabilityCount {
		t.Errorf("semantic review has %d unique IDs, want %d", len(seen), expectedSemanticCapabilityCount)
	}
	if computed != review.Summary {
		t.Errorf("semantic review summary = %+v, recomputed %+v", review.Summary, computed)
	}
	validateCAP045Disclosure(t, review.Records)
}

func readSemanticLedger(t *testing.T, root, relativePath string) map[string]capabilityRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relativePath)))
	if err != nil {
		t.Fatalf("read semantic source ledger: %v", err)
	}
	var got ledger
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode semantic source ledger: %v", err)
	}
	records := make(map[string]capabilityRecord, len(got.Records))
	for _, record := range got.Records {
		records[record.CapabilityID] = record
	}
	return records
}

func validateIndependentReviewer(t *testing.T, reviewer semanticReviewer) {
	t.Helper()
	id := strings.ToLower(strings.TrimSpace(reviewer.ID))
	generic := map[string]struct{}{
		"": {}, "codex": {}, "reviewer": {}, "agent-team": {}, "codex-agent-team": {}, "unknown": {}, "tbd": {},
	}
	if _, rejected := generic[id]; rejected || strings.Contains(id, "agent-team") ||
		!strings.Contains(id, "cap045") || !strings.Contains(id, "independent") {
		t.Errorf("semantic reviewer ID %q is generic or not review-specific", reviewer.ID)
	}
	if reviewer.Role != "independent-semantic-auditor" {
		t.Errorf("semantic reviewer role = %q", reviewer.Role)
	}
	if reviewer.TaskPath != "/root/subagent_runmode_map" {
		t.Errorf("semantic reviewer taskPath = %q", reviewer.TaskPath)
	}
	statement := strings.ToLower(strings.TrimSpace(reviewer.IndependenceStatement))
	if len(statement) < 100 || len(statement) > maximumSemanticEvidenceBytes ||
		!strings.Contains(statement, "did not copy") || !strings.Contains(statement, "status") {
		t.Errorf("semantic reviewer independence statement is incomplete or oversized")
	}
	disclosure := strings.ToLower(strings.TrimSpace(reviewer.PriorAccessDisclosure))
	if len(disclosure) < 80 || len(disclosure) > maximumSemanticEvidenceBytes ||
		!strings.Contains(disclosure, "prior access") || !strings.Contains(disclosure, "not a clean-room certification") {
		t.Errorf("semantic reviewer prior-access disclosure is incomplete or oversized")
	}
}

func validateUniquePaths(t *testing.T, root, label string, paths []string) {
	t.Helper()
	validatePaths(t, root, label, paths)
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if _, duplicate := seen[path]; duplicate {
			t.Errorf("%s contains duplicate path %q", label, path)
		}
		seen[path] = struct{}{}
	}
}

func requireReviewedPaths(t *testing.T, label string, required, reviewed []string) {
	t.Helper()
	seen := make(map[string]struct{}, len(reviewed))
	for _, path := range reviewed {
		seen[path] = struct{}{}
	}
	for _, path := range required {
		if _, ok := seen[path]; !ok {
			t.Errorf("%s did not review ledger path %q", label, path)
		}
	}
}

func validateSemanticCriterion(t *testing.T, label string, criterion semanticCriterion) {
	t.Helper()
	switch criterion.Verdict {
	case "pass", "partial", "fail":
	default:
		t.Errorf("%s verdict %q is invalid", label, criterion.Verdict)
	}
	if strings.TrimSpace(criterion.Evidence) == "" || len(criterion.Evidence) > maximumSemanticEvidenceBytes {
		t.Errorf("%s evidence is empty or oversized", label)
	}
}

func validateSemanticLimitations(t *testing.T, id string, limitations []string) {
	t.Helper()
	if len(limitations) > maximumSemanticLimitations {
		t.Errorf("%s has %d residual limitations, want at most %d", id, len(limitations), maximumSemanticLimitations)
	}
	for index, limitation := range limitations {
		if strings.TrimSpace(limitation) == "" || len(limitation) > maximumSemanticLimitationBytes {
			t.Errorf("%s residualLimitations[%d] is empty or oversized", id, index)
		}
	}
}

func validateSemanticFindings(t *testing.T, record semanticReviewRecord) {
	t.Helper()
	if len(record.ReviewFindings) > maximumSemanticLimitations {
		t.Errorf("%s has %d review findings, want at most %d", record.CapabilityID, len(record.ReviewFindings), maximumSemanticLimitations)
	}
	seen := make(map[string]struct{}, len(record.ReviewFindings))
	for index, finding := range record.ReviewFindings {
		label := fmt.Sprintf("%s reviewFindings[%d]", record.CapabilityID, index)
		if strings.TrimSpace(finding.ID) == "" || len(finding.ID) > 128 {
			t.Errorf("%s ID is empty or oversized", label)
		}
		if _, duplicate := seen[finding.ID]; duplicate {
			t.Errorf("%s duplicates finding ID %q", record.CapabilityID, finding.ID)
		}
		seen[finding.ID] = struct{}{}
		if _, err := time.Parse(time.RFC3339, finding.ObservedAt); err != nil {
			t.Errorf("%s observedAt %q is invalid: %v", label, finding.ObservedAt, err)
		}
		switch finding.Severity {
		case "low", "medium", "high":
		default:
			t.Errorf("%s severity %q is invalid", label, finding.Severity)
		}
		switch finding.Criterion {
		case "contract", "happyPath", "negativePath", "composition", "provenance":
		default:
			t.Errorf("%s criterion %q is invalid", label, finding.Criterion)
		}
		if strings.TrimSpace(finding.Evidence) == "" || len(finding.Evidence) > maximumSemanticEvidenceBytes {
			t.Errorf("%s evidence is empty or oversized", label)
		}
		switch finding.Status {
		case "open":
			if finding.ResolutionEvidence != "" {
				t.Errorf("%s open finding has resolution evidence", label)
			}
			if finding.Severity == "high" && record.OverallVerdict != "fail" {
				t.Errorf("%s open high finding requires overall fail", label)
			}
		case "resolved":
			if strings.TrimSpace(finding.ResolutionEvidence) == "" || len(finding.ResolutionEvidence) > maximumSemanticEvidenceBytes {
				t.Errorf("%s resolved finding lacks bounded resolution evidence", label)
			}
		default:
			t.Errorf("%s status %q is invalid", label, finding.Status)
		}
	}
}

func validateVerdictStatusCompatibility(t *testing.T, record semanticReviewRecord, ledgerStatus string, partial, failed bool) {
	t.Helper()
	switch ledgerStatus {
	case "verified":
		if record.OverallVerdict != "pass" || partial || failed {
			t.Errorf("%s is verified in the ledger but semantic verdict is %q", record.CapabilityID, record.OverallVerdict)
		}
	case "connected":
		// A semantic review is allowed to be newer than the owned matrix/ledger:
		// an all-pass record is the bounded promotion signal, while the observed
		// source status remains recorded truthfully until its owner promotes it.
		compatible := record.OverallVerdict == "pass" && !partial && !failed
		compatible = compatible || record.OverallVerdict == "pass-with-limitations" && partial && !failed
		compatible = compatible || record.OverallVerdict == "fail" && failed
		if !compatible {
			t.Errorf("%s is connected in the ledger but semantic verdict %q is incompatible with partial=%v fail=%v", record.CapabilityID, record.OverallVerdict, partial, failed)
		}
	default:
		t.Errorf("%s ledger status %q is unsupported", record.CapabilityID, ledgerStatus)
	}
}

func matrixStatusForLedger(status string) string {
	switch status {
	case "verified":
		return "Verified"
	case "connected":
		return "Connected"
	default:
		return ""
	}
}

func validateCAP045Disclosure(t *testing.T, records []semanticReviewRecord) {
	t.Helper()
	for _, record := range records {
		if record.CapabilityID != "CAP-045" {
			continue
		}
		if record.OverallVerdict != "pass-with-limitations" || record.Criteria.Provenance.Verdict != "partial" {
			t.Errorf("CAP-045 must remain pass-with-limitations with partial provenance")
		}
		joined := strings.ToLower(strings.Join(record.ResidualLimitations, " "))
		if !strings.Contains(joined, "prior access") || !strings.Contains(joined, "clean-room") ||
			!strings.Contains(joined, "not reproducible") {
			t.Errorf("CAP-045 must disclose prior access, clean-room limits, and non-reproducible historical scan")
		}
		return
	}
	t.Error("semantic review is missing CAP-045 disclosure")
}
