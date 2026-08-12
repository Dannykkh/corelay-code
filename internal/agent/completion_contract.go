package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	completionContractVersion       = 1
	maxCompletionRunIDBytes         = 256
	maxCompletionCriteria           = 256
	maxCompletionCriterionTextBytes = 4 * 1024
	maxCompletionCriterionIDBytes   = 96
	maxCompletionSummaryBytes       = 1024
)

// CompletionClaimState is the per-run state of one Definition of Done item.
type CompletionClaimState string

const (
	CompletionClaimPending   CompletionClaimState = "pending"
	CompletionClaimSatisfied CompletionClaimState = "satisfied"
	CompletionClaimBlocked   CompletionClaimState = "blocked"
)

// CompletionContractStatus is derived from all criterion states. A blocked
// criterion takes precedence; completion requires every criterion satisfied.
type CompletionContractStatus string

const (
	CompletionStatusIncomplete CompletionContractStatus = "incomplete"
	CompletionStatusComplete   CompletionContractStatus = "complete"
	CompletionStatusBlocked    CompletionContractStatus = "blocked"
)

// CompletionContractErrorCode allows adapters to fail closed without parsing
// error strings or retaining rejected claim content.
type CompletionContractErrorCode string

const (
	CompletionErrorInvalidContract    CompletionContractErrorCode = "invalid_contract"
	CompletionErrorInvalidField       CompletionContractErrorCode = "invalid_field"
	CompletionErrorFieldTooLarge      CompletionContractErrorCode = "field_too_large"
	CompletionErrorUnknownCriterion   CompletionContractErrorCode = "unknown_criterion"
	CompletionErrorDuplicateCriterion CompletionContractErrorCode = "duplicate_criterion"
	CompletionErrorDuplicateClaim     CompletionContractErrorCode = "duplicate_claim"
	CompletionErrorConflictingClaim   CompletionContractErrorCode = "conflicting_claim"
	CompletionErrorInvalidClaimState  CompletionContractErrorCode = "invalid_claim_state"
	CompletionErrorInvalidEvidence    CompletionContractErrorCode = "invalid_evidence_digest"
	CompletionErrorEvidenceRequired   CompletionContractErrorCode = "evidence_required"
	CompletionErrorEvidenceResolver   CompletionContractErrorCode = "evidence_resolver_unavailable"
	CompletionErrorEvidenceMismatch   CompletionContractErrorCode = "evidence_mismatch"
	CompletionErrorStaleRevision      CompletionContractErrorCode = "stale_revision"
	CompletionErrorRevisionExhausted  CompletionContractErrorCode = "revision_exhausted"
	CompletionErrorEmptyClaims        CompletionContractErrorCode = "empty_claims"
)

// CompletionContractError contains only bounded identifiers selected by this
// package. Rejected summaries, evidence payloads, and resolver errors are not
// copied into the error boundary.
type CompletionContractError struct {
	Code        CompletionContractErrorCode `json:"code"`
	Field       string                      `json:"field,omitempty"`
	CriterionID string                      `json:"criterionId,omitempty"`
}

func (e *CompletionContractError) Error() string {
	if e == nil {
		return ""
	}
	message := "completion contract: " + string(e.Code)
	if e.Field != "" {
		message += " (field=" + e.Field + ")"
	}
	if e.CriterionID != "" {
		message += " (criterion=" + e.CriterionID + ")"
	}
	return message
}

func completionContractError(code CompletionContractErrorCode, field, criterionID string) error {
	return &CompletionContractError{Code: code, Field: field, CriterionID: criterionID}
}

// CompletionContractSpec binds one immutable PlanAnchor contract to one run.
// EvidenceNotRequiredCriteria contains exact Definition of Done text (anchor
// whitespace normalization is applied) and is rejected if unknown or repeated.
type CompletionContractSpec struct {
	RunID                       string
	PlanAnchor                  PlanAnchor
	EvidenceNotRequiredCriteria []string
}

// CompletionClaim carries either digest-bound evidence metadata, a blocked
// reason, or an explicit assertion for an evidence-not-required criterion.
// Raw evidence has no field here.
type CompletionClaim struct {
	CriterionID    string               `json:"criterionId"`
	State          CompletionClaimState `json:"state"`
	EvidenceDigest string               `json:"evidenceDigest,omitempty"`
	Summary        string               `json:"summary,omitempty"`
	Assertion      string               `json:"assertion,omitempty"`
}

// CompletionEvidenceResolver confirms that a digest is present in the active
// run's evidence store. It receives no raw evidence content.
type CompletionEvidenceResolver func(runID, evidenceDigest string) (bool, error)

type completionCriterion struct {
	id               string
	text             string
	evidenceRequired bool
	state            CompletionClaimState
	evidenceDigest   string
	summary          string
	assertion        string
	claimRevision    uint64
}

// CompletionContract is a small, run-owned CAS state machine. Criterion text
// and membership never change after construction; ApplyClaims is the only
// mutation point and commits a whole claim batch atomically.
type CompletionContract struct {
	mu sync.RWMutex

	runID        string
	planRevision int
	revision     uint64
	criteria     []completionCriterion
	criterionIdx map[string]int
}

// CompletionCriterionID derives the stable identifier used by claims. It is
// based only on the normalized immutable criterion text, so reordering a Plan
// Anchor does not change the identifier.
func CompletionCriterionID(text string) (string, error) {
	if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
		return "", completionContractError(CompletionErrorInvalidField, "criterionText", "")
	}
	if len(text) > maxCompletionCriterionTextBytes {
		return "", completionContractError(CompletionErrorFieldTooLarge, "criterionText", "")
	}
	normalized := normalizeAnchorText(text)
	if normalized == "" {
		return "", completionContractError(CompletionErrorInvalidField, "criterionText", "")
	}
	digest := sha256.Sum256([]byte(normalized))
	return "criterion:sha256:" + hex.EncodeToString(digest[:]), nil
}

// NewCompletionContract copies all caller-owned input and creates pending
// criteria in the PlanAnchor's deterministic order.
func NewCompletionContract(spec CompletionContractSpec) (*CompletionContract, error) {
	runID := strings.TrimSpace(spec.RunID)
	if runID == "" || !utf8.ValidString(runID) || containsCompletionControl(runID) {
		return nil, completionContractError(CompletionErrorInvalidField, "runId", "")
	}
	if len(runID) > maxCompletionRunIDBytes {
		return nil, completionContractError(CompletionErrorFieldTooLarge, "runId", "")
	}
	if !spec.PlanAnchor.Valid() {
		return nil, completionContractError(CompletionErrorInvalidContract, "planAnchor", "")
	}

	definitionOfDone := spec.PlanAnchor.DefinitionOfDone()
	if len(definitionOfDone) == 0 {
		return nil, completionContractError(CompletionErrorInvalidContract, "definitionOfDone", "")
	}
	if len(definitionOfDone) > maxCompletionCriteria {
		return nil, completionContractError(CompletionErrorFieldTooLarge, "definitionOfDone", "")
	}

	optionalByText := make(map[string]struct{}, len(spec.EvidenceNotRequiredCriteria))
	knownText := make(map[string]struct{}, len(definitionOfDone))
	for _, text := range definitionOfDone {
		knownText[text] = struct{}{}
	}
	for _, text := range spec.EvidenceNotRequiredCriteria {
		if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
			return nil, completionContractError(CompletionErrorInvalidField, "evidenceNotRequiredCriteria", "")
		}
		if len(text) > maxCompletionCriterionTextBytes {
			return nil, completionContractError(CompletionErrorFieldTooLarge, "evidenceNotRequiredCriteria", "")
		}
		text = normalizeAnchorText(text)
		if _, ok := knownText[text]; !ok {
			return nil, completionContractError(CompletionErrorUnknownCriterion, "evidenceNotRequiredCriteria", "")
		}
		if _, duplicate := optionalByText[text]; duplicate {
			return nil, completionContractError(CompletionErrorDuplicateCriterion, "evidenceNotRequiredCriteria", "")
		}
		optionalByText[text] = struct{}{}
	}

	contract := &CompletionContract{
		runID:        runID,
		planRevision: spec.PlanAnchor.Revision(),
		criteria:     make([]completionCriterion, 0, len(definitionOfDone)),
		criterionIdx: make(map[string]int, len(definitionOfDone)),
	}
	for _, text := range definitionOfDone {
		if !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
			return nil, completionContractError(CompletionErrorInvalidField, "criterionText", "")
		}
		if len(text) > maxCompletionCriterionTextBytes {
			return nil, completionContractError(CompletionErrorFieldTooLarge, "criterionText", "")
		}
		id, err := CompletionCriterionID(text)
		if err != nil {
			return nil, err
		}
		if existing, collision := contract.criterionIdx[id]; collision {
			code := CompletionErrorDuplicateCriterion
			if contract.criteria[existing].text != text {
				code = CompletionErrorInvalidContract
			}
			return nil, completionContractError(code, "definitionOfDone", id)
		}
		_, evidenceNotRequired := optionalByText[text]
		contract.criterionIdx[id] = len(contract.criteria)
		contract.criteria = append(contract.criteria, completionCriterion{
			id:               id,
			text:             text,
			evidenceRequired: !evidenceNotRequired,
			state:            CompletionClaimPending,
		})
	}
	return contract, nil
}

// Revision is the monotonic contract CAS revision.
func (c *CompletionContract) Revision() uint64 {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.revision
}

// Status derives the current terminal disposition without changing state.
func (c *CompletionContract) Status() CompletionContractStatus {
	if c == nil {
		return CompletionStatusIncomplete
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return completionStatusLocked(c.criteria)
}

// CriterionText returns the exact immutable PlanAnchor criterion text. Receipt
// serialization uses Snapshot instead, which redacts this value.
func (c *CompletionContract) CriterionText(criterionID string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	index, ok := c.criterionIdx[criterionID]
	if !ok {
		return "", false
	}
	return c.criteria[index].text, true
}

type preparedCompletionClaim struct {
	criterionIndex int
	criterionID    string
	state          CompletionClaimState
	evidenceDigest string
	summary        string
	assertion      string
}

// ApplyClaims validates a complete batch, verifies every referenced evidence
// digest outside the lock, then re-checks expectedRevision and commits once.
// Any failure leaves the contract byte-for-byte unchanged.
func (c *CompletionContract) ApplyClaims(
	expectedRevision uint64,
	claims []CompletionClaim,
	resolveEvidence CompletionEvidenceResolver,
) error {
	if c == nil {
		return completionContractError(CompletionErrorInvalidContract, "contract", "")
	}

	c.mu.RLock()
	if expectedRevision != c.revision {
		c.mu.RUnlock()
		return completionContractError(CompletionErrorStaleRevision, "expectedRevision", "")
	}
	if c.revision == ^uint64(0) {
		c.mu.RUnlock()
		return completionContractError(CompletionErrorRevisionExhausted, "revision", "")
	}
	prepared, err := c.prepareClaimsLocked(claims)
	runID := c.runID
	c.mu.RUnlock()
	if err != nil {
		return err
	}

	for _, claim := range prepared {
		if claim.state != CompletionClaimSatisfied || claim.evidenceDigest == "" {
			continue
		}
		if resolveEvidence == nil {
			return completionContractError(CompletionErrorEvidenceResolver, "evidenceDigest", claim.criterionID)
		}
		found, resolverErr := resolveEvidence(runID, claim.evidenceDigest)
		if resolverErr != nil {
			return completionContractError(CompletionErrorEvidenceResolver, "evidenceDigest", claim.criterionID)
		}
		if !found {
			return completionContractError(CompletionErrorEvidenceMismatch, "evidenceDigest", claim.criterionID)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.revision != expectedRevision {
		return completionContractError(CompletionErrorStaleRevision, "expectedRevision", "")
	}
	nextRevision := c.revision + 1
	for _, claim := range prepared {
		criterion := &c.criteria[claim.criterionIndex]
		criterion.state = claim.state
		criterion.evidenceDigest = claim.evidenceDigest
		criterion.summary = claim.summary
		criterion.assertion = claim.assertion
		criterion.claimRevision = nextRevision
	}
	c.revision = nextRevision
	return nil
}

func (c *CompletionContract) prepareClaimsLocked(claims []CompletionClaim) ([]preparedCompletionClaim, error) {
	if len(claims) == 0 {
		return nil, completionContractError(CompletionErrorEmptyClaims, "claims", "")
	}
	if len(claims) > maxCompletionCriteria {
		return nil, completionContractError(CompletionErrorFieldTooLarge, "claims", "")
	}
	prepared := make([]preparedCompletionClaim, 0, len(claims))
	seen := make(map[string]preparedCompletionClaim, len(claims))
	for _, claim := range claims {
		item, err := c.prepareClaimLocked(claim)
		if err != nil {
			return nil, err
		}
		if previous, duplicate := seen[item.criterionID]; duplicate {
			code := CompletionErrorDuplicateClaim
			if !samePreparedCompletionClaim(previous, item) {
				code = CompletionErrorConflictingClaim
			}
			return nil, completionContractError(code, "claims", item.criterionID)
		}
		seen[item.criterionID] = item
		prepared = append(prepared, item)
	}
	return prepared, nil
}

func (c *CompletionContract) prepareClaimLocked(claim CompletionClaim) (preparedCompletionClaim, error) {
	criterionID := strings.TrimSpace(claim.CriterionID)
	if criterionID == "" || criterionID != claim.CriterionID || !utf8.ValidString(criterionID) || containsCompletionControl(criterionID) {
		return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidField, "criterionId", "")
	}
	if len(criterionID) > maxCompletionCriterionIDBytes {
		return preparedCompletionClaim{}, completionContractError(CompletionErrorFieldTooLarge, "criterionId", "")
	}
	if !validCompletionCriterionID(criterionID) {
		return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidField, "criterionId", "")
	}
	criterionIndex, known := c.criterionIdx[criterionID]
	if !known {
		return preparedCompletionClaim{}, completionContractError(CompletionErrorUnknownCriterion, "criterionId", criterionID)
	}
	criterion := c.criteria[criterionIndex]

	state := claim.State
	switch state {
	case CompletionClaimPending, CompletionClaimSatisfied, CompletionClaimBlocked:
	default:
		return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidClaimState, "state", criterionID)
	}

	evidenceDigest := strings.TrimSpace(claim.EvidenceDigest)
	if evidenceDigest != claim.EvidenceDigest {
		return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidEvidence, "evidenceDigest", criterionID)
	}
	if evidenceDigest != "" && !validCompletionEvidenceDigest(evidenceDigest) {
		return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidEvidence, "evidenceDigest", criterionID)
	}

	if !utf8.ValidString(claim.Summary) {
		return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidField, "summary", criterionID)
	}
	if len(claim.Summary) > maxCompletionSummaryBytes {
		return preparedCompletionClaim{}, completionContractError(CompletionErrorFieldTooLarge, "summary", criterionID)
	}
	summary := completionReceiptText(strings.TrimSpace(claim.Summary), maxCompletionSummaryBytes)
	if !utf8.ValidString(claim.Assertion) {
		return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidField, "assertion", criterionID)
	}
	if len(claim.Assertion) > maxCompletionSummaryBytes {
		return preparedCompletionClaim{}, completionContractError(CompletionErrorFieldTooLarge, "assertion", criterionID)
	}
	assertion := completionReceiptText(strings.TrimSpace(claim.Assertion), maxCompletionSummaryBytes)

	switch state {
	case CompletionClaimPending:
		if evidenceDigest != "" || summary != "" || assertion != "" {
			return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidField, "pendingClaim", criterionID)
		}
	case CompletionClaimBlocked:
		if evidenceDigest != "" || summary == "" || assertion != "" {
			return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidField, "blockedClaim", criterionID)
		}
	case CompletionClaimSatisfied:
		if criterion.evidenceRequired && evidenceDigest == "" {
			return preparedCompletionClaim{}, completionContractError(CompletionErrorEvidenceRequired, "evidenceDigest", criterionID)
		}
		if criterion.evidenceRequired && assertion != "" {
			return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidField, "assertion", criterionID)
		}
		if !criterion.evidenceRequired && evidenceDigest == "" && assertion == "" {
			return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidField, "assertion", criterionID)
		}
		if !criterion.evidenceRequired && evidenceDigest == "" && summary != "" {
			return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidField, "summary", criterionID)
		}
		if evidenceDigest != "" && assertion != "" {
			return preparedCompletionClaim{}, completionContractError(CompletionErrorInvalidField, "assertion", criterionID)
		}
	}

	item := preparedCompletionClaim{
		criterionIndex: criterionIndex,
		criterionID:    criterionID,
		state:          state,
		evidenceDigest: evidenceDigest,
		summary:        summary,
		assertion:      assertion,
	}
	if state == criterion.state {
		code := CompletionErrorDuplicateClaim
		if criterion.evidenceDigest != evidenceDigest || criterion.summary != summary || criterion.assertion != assertion {
			code = CompletionErrorConflictingClaim
		}
		return preparedCompletionClaim{}, completionContractError(code, "claim", criterionID)
	}
	return item, nil
}

func samePreparedCompletionClaim(left, right preparedCompletionClaim) bool {
	return left.criterionID == right.criterionID &&
		left.state == right.state &&
		left.evidenceDigest == right.evidenceDigest &&
		left.summary == right.summary &&
		left.assertion == right.assertion
}

func validCompletionEvidenceDigest(value string) bool {
	return validLowerSHA256(value, "sha256:")
}

func validCompletionCriterionID(value string) bool {
	return validLowerSHA256(value, "criterion:sha256:")
}

func validLowerSHA256(value, prefix string) bool {
	if len(value) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(value, prefix) {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil && value == strings.ToLower(value)
}

func containsCompletionControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}

func completionReceiptText(value string, limit int) string {
	value = redactSensitiveString(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func completionStatusLocked(criteria []completionCriterion) CompletionContractStatus {
	if len(criteria) == 0 {
		return CompletionStatusIncomplete
	}
	allSatisfied := true
	for _, criterion := range criteria {
		switch criterion.state {
		case CompletionClaimBlocked:
			return CompletionStatusBlocked
		case CompletionClaimSatisfied:
		default:
			allSatisfied = false
		}
	}
	if allSatisfied {
		return CompletionStatusComplete
	}
	return CompletionStatusIncomplete
}

// CompletionCriterionSnapshot is deterministic receipt metadata. Text and
// Summary are redacted; no evidence payload is retained.
type CompletionCriterionSnapshot struct {
	ID               string               `json:"id"`
	Text             string               `json:"text"`
	EvidenceRequired bool                 `json:"evidenceRequired"`
	State            CompletionClaimState `json:"state"`
	EvidenceDigest   string               `json:"evidenceDigest,omitempty"`
	Summary          string               `json:"summary,omitempty"`
	Assertion        string               `json:"assertion,omitempty"`
	ClaimRevision    uint64               `json:"claimRevision"`
}

// CompletionContractSnapshot uses structs and an ordered slice only, making
// json.Marshal byte-stable for the same contract revision.
type CompletionContractSnapshot struct {
	Version      int                           `json:"version"`
	RunID        string                        `json:"runId"`
	PlanRevision int                           `json:"planRevision"`
	Revision     uint64                        `json:"revision"`
	Status       CompletionContractStatus      `json:"status"`
	Criteria     []CompletionCriterionSnapshot `json:"criteria"`
}

func (c *CompletionContract) Snapshot() (CompletionContractSnapshot, error) {
	if c == nil {
		return CompletionContractSnapshot{}, completionContractError(CompletionErrorInvalidContract, "contract", "")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.runID == "" || len(c.criteria) == 0 || len(c.criterionIdx) != len(c.criteria) {
		return CompletionContractSnapshot{}, completionContractError(CompletionErrorInvalidContract, "contract", "")
	}
	snapshot := CompletionContractSnapshot{
		Version:      completionContractVersion,
		RunID:        completionReceiptText(c.runID, maxCompletionRunIDBytes),
		PlanRevision: c.planRevision,
		Revision:     c.revision,
		Status:       completionStatusLocked(c.criteria),
		Criteria:     make([]CompletionCriterionSnapshot, 0, len(c.criteria)),
	}
	for _, criterion := range c.criteria {
		snapshot.Criteria = append(snapshot.Criteria, CompletionCriterionSnapshot{
			ID:               criterion.id,
			Text:             completionReceiptText(criterion.text, maxCompletionCriterionTextBytes),
			EvidenceRequired: criterion.evidenceRequired,
			State:            criterion.state,
			EvidenceDigest:   criterion.evidenceDigest,
			Summary:          criterion.summary,
			Assertion:        criterion.assertion,
			ClaimRevision:    criterion.claimRevision,
		})
	}
	return snapshot, nil
}

func (c *CompletionContract) SnapshotJSON() ([]byte, error) {
	snapshot, err := c.Snapshot()
	if err != nil {
		return nil, err
	}
	return json.Marshal(snapshot)
}

// MarshalJSON deliberately exposes only the receipt-safe snapshot.
func (c *CompletionContract) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("null"), nil
	}
	return c.SnapshotJSON()
}
