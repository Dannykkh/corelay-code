package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	reportCompletionToolName      = "ReportCompletion"
	maxCompletionReportInputBytes = 1 << 20
	reportCompletionToolSchema    = `{
  "type": "object",
  "properties": {
    "expectedRevision": {"type": "integer", "minimum": 0},
    "claims": {
      "type": "array",
      "minItems": 1,
      "maxItems": 256,
      "uniqueItems": true,
      "items": {
        "type": "object",
        "properties": {
          "criterionId": {"type": "string", "minLength": 1, "maxLength": 96},
          "state": {"type": "string", "enum": ["pending", "satisfied", "blocked"]},
          "evidenceDigest": {"type": "string", "maxLength": 71},
          "summary": {"type": "string", "maxLength": 1024},
          "assertion": {"type": "string", "maxLength": 1024}
        },
        "required": ["criterionId", "state"],
        "additionalProperties": false
      }
    }
  },
  "required": ["expectedRevision", "claims"],
  "additionalProperties": false
}`
)

// ReportCompletionToolDef returns the fixed internal state-transition tool.
// It is intentionally absent from default catalogs: strict-run composition
// must append it explicitly after creating a run-owned CompletionContract.
func ReportCompletionToolDef() types.ToolDef {
	return types.ToolDef{
		Name: reportCompletionToolName,
		Description: "Atomically report evidence-bound completion claims against the active run contract. " +
			"Use the current contract revision and criterion IDs exactly.",
		InputSchema: json.RawMessage(reportCompletionToolSchema),
	}
}

// completionReportCatalogProof is created only after a valid immutable
// catalog identity envelope has been unwrapped. Keeping it private prevents a
// legacy/direct ExecuteTool caller from opting itself into this state change.
type completionReportCatalogProof struct {
	runID    string
	identity toolExecutorIdentity
}

func newCompletionReportCatalogProof(
	identity toolExecutorIdentity,
	runID string,
) (completionReportCatalogProof, bool) {
	runID = strings.TrimSpace(runID)
	if runID == "" || identity.ToolName != reportCompletionToolName ||
		identity.Kind != toolExecutorBuiltIn || identity.Owner != "corelaycode" ||
		identity.ExecutorID != "builtin:"+reportCompletionToolName ||
		identity.SchemaDigest != toolSchemaDigest(ReportCompletionToolDef().InputSchema) ||
		validateBoundToolExecutorIdentity(identity) != nil {
		return completionReportCatalogProof{}, false
	}
	return completionReportCatalogProof{runID: runID, identity: identity}, true
}

func (proof *completionReportCatalogProof) validFor(
	contract *CompletionContract,
	expectedRunID string,
) bool {
	if proof == nil || contract == nil || strings.TrimSpace(expectedRunID) == "" ||
		proof.runID != strings.TrimSpace(expectedRunID) ||
		proof.identity.ToolName != reportCompletionToolName ||
		proof.identity.SchemaDigest != toolSchemaDigest(ReportCompletionToolDef().InputSchema) ||
		validateBoundToolExecutorIdentity(proof.identity) != nil {
		return false
	}
	contract.mu.RLock()
	defer contract.mu.RUnlock()
	return contract.runID == proof.runID
}

type completionReportRequest struct {
	ExpectedRevision *uint64           `json:"expectedRevision"`
	Claims           []CompletionClaim `json:"claims"`
}

type completionReportReceipt struct {
	Revision uint64                   `json:"revision"`
	Status   CompletionContractStatus `json:"status"`
	Count    int                      `json:"count"`
}

func completionReportReceiptJSON(contract *CompletionContract, count int) string {
	receipt := completionReportReceipt{
		Status: CompletionStatusIncomplete,
		Count:  count,
	}
	if contract != nil {
		if snapshot, err := contract.Snapshot(); err == nil {
			receipt.Revision = snapshot.Revision
			receipt.Status = snapshot.Status
		}
	}
	encoded, _ := json.Marshal(receipt)
	return string(encoded)
}

func decodeCompletionReportRequest(input json.RawMessage) (completionReportRequest, bool) {
	if len(input) == 0 || len(input) > maxCompletionReportInputBytes {
		return completionReportRequest{}, false
	}
	value, err := decodeStrictJSON(input, maxToolSchemaDepth)
	if err != nil {
		return completionReportRequest{}, false
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return completionReportRequest{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	var request completionReportRequest
	if err := decoder.Decode(&request); err != nil || request.ExpectedRevision == nil {
		return completionReportRequest{}, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return completionReportRequest{}, false
	}
	return request, true
}

func executeReportCompletion(input json.RawMessage, opts ToolExecutionOptions) (string, bool) {
	if opts.CompletionContract == nil || opts.CompletionEvidenceResolver == nil ||
		!opts.completionReportProof.validFor(opts.CompletionContract, opts.ExpectedRunID) {
		return completionReportReceiptJSON(nil, 0), true
	}
	request, ok := decodeCompletionReportRequest(input)
	if !ok {
		return completionReportReceiptJSON(opts.CompletionContract, 0), true
	}
	if err := opts.CompletionContract.ApplyClaims(
		*request.ExpectedRevision,
		request.Claims,
		opts.CompletionEvidenceResolver,
	); err != nil {
		return completionReportReceiptJSON(opts.CompletionContract, 0), true
	}
	return completionReportReceiptJSON(opts.CompletionContract, len(request.Claims)), false
}
