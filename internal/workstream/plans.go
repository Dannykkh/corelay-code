package workstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	CurrentPlanVersion       = 1
	maxPlanDefinitionBytes   = 8 << 20
	PlanStatusDraft          = "draft"
	PlanStatusApproved       = "approved"
	PlanStatusExecuting      = "executing"
	PlanStatusCompleted      = "completed"
	PlanStatusFailed         = "failed"
	PlanStageStatusPending   = "pending"
	PlanStageStatusRunning   = "running"
	PlanStageStatusCompleted = "completed"
	PlanStageStatusFailed    = "failed"
	PlanStageAttemptRunning  = "running"
	PlanStageAttemptComplete = "completed"
	PlanStageAttemptFailed   = "failed"
)

var (
	ErrPlanNotFound         = errors.New("workstream: plan not found")
	ErrPlanInvalid          = errors.New("workstream: invalid plan")
	ErrPlanCorrupt          = errors.New("workstream: stored plan requires recovery")
	ErrPlanConflict         = errors.New("workstream: plan already exists")
	ErrPlanRevisionConflict = errors.New("workstream: plan revision conflict")
	ErrPlanTransition       = errors.New("workstream: invalid plan transition")
	ErrPlanEvidence         = errors.New("workstream: invalid plan acceptance evidence")
	ErrPlanStageActive      = errors.New("workstream: plan stage execution is still active")
	planStoreLocks          sync.Map
)

// Plan is the canonical revisioned workflow record stored beneath one
// Workstream. Definition retains the validated TeamPlan execution contract;
// Stages contains the durable lifecycle state for those stage IDs.
type Plan struct {
	Version          int             `json:"version"`
	ID               string          `json:"id"`
	WorkstreamID     string          `json:"workstreamId"`
	Revision         uint64          `json:"revision"`
	StateRevision    uint64          `json:"stateRevision"`
	Status           string          `json:"status"`
	ApprovedRevision uint64          `json:"approvedRevision,omitempty"`
	Definition       json.RawMessage `json:"definition"`
	Stages           []PlanStage     `json:"stages"`
	CreatedAt        time.Time       `json:"createdAt"`
	UpdatedAt        time.Time       `json:"updatedAt"`
}

// PlanStage stores progress independently from the immutable TeamPlan stage
// definition. Its ID must exist in Plan.Definition.stages.
type PlanStage struct {
	ID       string             `json:"id"`
	Status   string             `json:"status"`
	Attempts []PlanStageAttempt `json:"attempts,omitempty"`
}

// PlanStageAttempt binds one execution attempt to an immutable definition
// revision. Only a server-produced receipt can supply completed evidence.
type PlanStageAttempt struct {
	RunID              string                  `json:"runId"`
	PlanRevision       uint64                  `json:"planRevision"`
	Status             string                  `json:"status"`
	StartedAt          time.Time               `json:"startedAt"`
	FinishedAt         time.Time               `json:"finishedAt,omitempty"`
	VerificationStatus string                  `json:"verificationStatus,omitempty"`
	ReceiptDigest      string                  `json:"receiptDigest,omitempty"`
	Evidence           *PlanAcceptanceEvidence `json:"evidence,omitempty"`
}

// PlanAcceptanceEvidence contains receipt-safe references only. The server
// computes the receipt and criteria digests after validating a run result.
type PlanAcceptanceEvidence struct {
	Source             string    `json:"source"`
	ReceiptDigest      string    `json:"receiptDigest"`
	CriteriaDigest     string    `json:"criteriaDigest"`
	VerificationStatus string    `json:"verificationStatus"`
	CompletionStatus   string    `json:"completionStatus"`
	RecordedAt         time.Time `json:"recordedAt"`
}

type CreatePlanRequest struct {
	ID         string
	Definition json.RawMessage
}

type UpdatePlanRequest struct {
	ExpectedRevision      uint64
	ExpectedStateRevision uint64
	Definition            json.RawMessage
}

type ApprovePlanRequest struct {
	ExpectedRevision      uint64
	ExpectedStateRevision uint64
}

type BeginPlanStageRequest struct {
	ExpectedRevision      uint64
	ExpectedStateRevision uint64
	StageID               string
	RunID                 string
}

type FinishPlanStageRequest struct {
	ExpectedRevision      uint64
	ExpectedStateRevision uint64
	StageID               string
	RunID                 string
	Status                string
	VerificationStatus    string
	ReceiptDigest         string
	CriteriaDigest        string
	EvidenceSource        string
	CompletionStatus      string
}

type ReconcilePlanStageRequest struct {
	ExpectedRevision      uint64
	ExpectedStateRevision uint64
	StageID               string
	RunID                 string
	ManualAcknowledged    bool
}

// CreatePlan persists the first revision of a workstream-owned plan. The
// caller validates the TeamPlan contract before passing its JSON definition.
func (s *Store) CreatePlan(workstreamID string, req CreatePlanRequest) (*Plan, error) {
	if s == nil {
		return nil, fmt.Errorf("workstream: store is unavailable")
	}
	workstreamID = strings.TrimSpace(workstreamID)
	if _, err := s.Get(workstreamID); err != nil {
		return nil, err
	}
	stageIDs, err := planDefinitionStageIDs(req.Definition)
	if err != nil {
		return nil, err
	}
	if len(req.Definition) > maxPlanDefinitionBytes {
		return nil, fmt.Errorf("%w: definition is too large", ErrPlanInvalid)
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = newPlanID(time.Now().UTC(), req.Definition)
	}
	if !validPlanID(id) {
		return nil, fmt.Errorf("%w: plan id", ErrPlanInvalid)
	}
	lock := planStoreLock(s.workspace, workstreamID)
	lock.Lock()
	defer lock.Unlock()
	if _, err := s.Get(workstreamID); err != nil {
		return nil, err
	}
	path := PlanPath(s.workspace, workstreamID, id)
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("%w: plan id already exists", ErrPlanConflict)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("workstream: inspect plan: %w", err)
	}
	now := time.Now().UTC()
	plan := &Plan{
		Version:       CurrentPlanVersion,
		ID:            id,
		WorkstreamID:  workstreamID,
		Revision:      1,
		StateRevision: 1,
		Status:        PlanStatusDraft,
		Definition:    append(json.RawMessage(nil), req.Definition...),
		Stages:        make([]PlanStage, 0, len(stageIDs)),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	for _, stageID := range stageIDs {
		plan.Stages = append(plan.Stages, PlanStage{ID: stageID, Status: PlanStageStatusPending})
	}
	if err := validatePlanRecord(*plan, workstreamID, id); err != nil {
		return nil, err
	}
	if err := writeJSONAtomic(path, plan); err != nil {
		return nil, err
	}
	s.mu.Lock()
	eventErr := s.appendEventLocked(workstreamID, TimelineEvent{
		Type:    "plan_created",
		Message: "Plan created",
		Data: map[string]string{
			"planId":   id,
			"revision": "1",
		},
	})
	s.mu.Unlock()
	if eventErr != nil {
		return nil, eventErr
	}
	return clonePlan(plan), nil
}

func (s *Store) GetPlan(workstreamID, planID string) (*Plan, error) {
	if s == nil {
		return nil, fmt.Errorf("workstream: store is unavailable")
	}
	workstreamID = strings.TrimSpace(workstreamID)
	if _, err := s.Get(workstreamID); err != nil {
		return nil, err
	}
	if !validPlanID(planID) {
		return nil, fmt.Errorf("%w: plan id", ErrPlanInvalid)
	}
	lock := planStoreLock(s.workspace, workstreamID)
	lock.Lock()
	defer lock.Unlock()
	return s.readPlanLocked(workstreamID, planID)
}

func (s *Store) ListPlans(workstreamID string) ([]Plan, error) {
	if s == nil {
		return nil, fmt.Errorf("workstream: store is unavailable")
	}
	workstreamID = strings.TrimSpace(workstreamID)
	if _, err := s.Get(workstreamID); err != nil {
		return nil, err
	}
	lock := planStoreLock(s.workspace, workstreamID)
	lock.Lock()
	defer lock.Unlock()
	entries, err := os.ReadDir(PlansDir(s.workspace, workstreamID))
	if os.IsNotExist(err) {
		return []Plan{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workstream: list plans: %w", err)
	}
	plans := make([]Plan, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		plan, err := s.readPlanLocked(workstreamID, id)
		if err != nil {
			return nil, err
		}
		plans = append(plans, *plan)
	}
	sort.Slice(plans, func(i, j int) bool {
		return plans[i].UpdatedAt.After(plans[j].UpdatedAt)
	})
	return plans, nil
}

// UpdatePlan replaces a not-yet-executed definition. Definition changes
// invalidate any approval and advance both definition and lifecycle revisions.
func (s *Store) UpdatePlan(workstreamID, planID string, req UpdatePlanRequest) (*Plan, error) {
	if s == nil {
		return nil, fmt.Errorf("workstream: store is unavailable")
	}
	workstreamID = strings.TrimSpace(workstreamID)
	if _, err := s.Get(workstreamID); err != nil {
		return nil, err
	}
	if !validPlanID(planID) {
		return nil, fmt.Errorf("%w: plan id", ErrPlanInvalid)
	}
	stageIDs, err := planDefinitionStageIDs(req.Definition)
	if err != nil {
		return nil, err
	}
	if len(req.Definition) > maxPlanDefinitionBytes {
		return nil, fmt.Errorf("%w: definition is too large", ErrPlanInvalid)
	}
	if req.ExpectedRevision == 0 || req.ExpectedStateRevision == 0 {
		return nil, fmt.Errorf("%w: expected revisions are required", ErrPlanRevisionConflict)
	}
	lock := planStoreLock(s.workspace, workstreamID)
	lock.Lock()
	defer lock.Unlock()
	plan, err := s.readPlanLocked(workstreamID, planID)
	if err != nil {
		return nil, err
	}
	if plan.Revision != req.ExpectedRevision || plan.StateRevision != req.ExpectedStateRevision {
		return nil, fmt.Errorf("%w: stale plan state", ErrPlanRevisionConflict)
	}
	if plan.Status != PlanStatusDraft && plan.Status != PlanStatusApproved {
		return nil, fmt.Errorf("%w: definition is immutable after execution starts", ErrPlanTransition)
	}
	if hasPlanStageAttempts(plan.Stages) {
		return nil, fmt.Errorf("%w: definition is immutable after execution starts", ErrPlanTransition)
	}
	if plan.Revision == ^uint64(0) || plan.StateRevision == ^uint64(0) {
		return nil, fmt.Errorf("%w: revision exhausted", ErrPlanRevisionConflict)
	}
	plan.Revision++
	plan.StateRevision++
	plan.Status = PlanStatusDraft
	plan.ApprovedRevision = 0
	plan.Definition = append(json.RawMessage(nil), req.Definition...)
	plan.Stages = make([]PlanStage, 0, len(stageIDs))
	for _, stageID := range stageIDs {
		plan.Stages = append(plan.Stages, PlanStage{ID: stageID, Status: PlanStageStatusPending})
	}
	plan.UpdatedAt = time.Now().UTC()
	if err := validatePlanRecord(*plan, workstreamID, planID); err != nil {
		return nil, err
	}
	if err := writeJSONAtomic(PlanPath(s.workspace, workstreamID, planID), plan); err != nil {
		return nil, err
	}
	s.recordPlanEvent(workstreamID, "plan_revised", "Plan definition revised", map[string]string{
		"planId": planID, "revision": fmt.Sprint(plan.Revision),
	})
	return clonePlan(plan), nil
}

// ApprovePlan binds approval to the exact current definition revision.
func (s *Store) ApprovePlan(workstreamID, planID string, req ApprovePlanRequest) (*Plan, error) {
	return s.mutatePlan(workstreamID, planID, req.ExpectedRevision, req.ExpectedStateRevision, func(plan *Plan) (string, string, map[string]string, error) {
		if plan.Status != PlanStatusDraft || plan.ApprovedRevision != 0 {
			return "", "", nil, fmt.Errorf("%w: only a draft plan can be approved", ErrPlanTransition)
		}
		if len(plan.Stages) == 0 {
			return "", "", nil, fmt.Errorf("%w: plan requires at least one stage", ErrPlanTransition)
		}
		if hasPlanStageAttempts(plan.Stages) {
			return "", "", nil, fmt.Errorf("%w: plan already has execution history", ErrPlanTransition)
		}
		plan.Status = PlanStatusApproved
		plan.ApprovedRevision = plan.Revision
		return "plan_approved", "Plan approved", nil, nil
	})
}

// BeginPlanStage atomically claims the next eligible stage for one run.
func (s *Store) BeginPlanStage(workstreamID, planID string, req BeginPlanStageRequest) (*Plan, error) {
	if !validPlanRunID(req.RunID) || !validPlanID(req.StageID) {
		return nil, fmt.Errorf("%w: stage or run id", ErrPlanInvalid)
	}
	return s.mutatePlan(workstreamID, planID, req.ExpectedRevision, req.ExpectedStateRevision, func(plan *Plan) (string, string, map[string]string, error) {
		if plan.ApprovedRevision != plan.Revision || (plan.Status != PlanStatusApproved && plan.Status != PlanStatusExecuting && plan.Status != PlanStatusFailed) {
			return "", "", nil, fmt.Errorf("%w: current plan revision is not approved for execution", ErrPlanTransition)
		}
		if strings.TrimSpace(plan.Status) == PlanStatusCompleted {
			return "", "", nil, fmt.Errorf("%w: completed plan is immutable", ErrPlanTransition)
		}
		if planHasRunningStage(plan.Stages) {
			return "", "", nil, fmt.Errorf("%w: another stage is already running", ErrPlanTransition)
		}
		index := planStageIndex(plan.Stages, req.StageID)
		if index < 0 {
			return "", "", nil, fmt.Errorf("%w: stage does not belong to plan", ErrPlanTransition)
		}
		stage := &plan.Stages[index]
		if stage.Status != PlanStageStatusPending && stage.Status != PlanStageStatusFailed {
			return "", "", nil, fmt.Errorf("%w: stage is not eligible to run", ErrPlanTransition)
		}
		for i := 0; i < index; i++ {
			if plan.Stages[i].Status != PlanStageStatusCompleted {
				return "", "", nil, fmt.Errorf("%w: preceding stages are incomplete", ErrPlanTransition)
			}
		}
		for _, candidate := range plan.Stages {
			for _, attempt := range candidate.Attempts {
				if attempt.RunID == req.RunID {
					return "", "", nil, fmt.Errorf("%w: run id already used by plan", ErrPlanTransition)
				}
			}
		}
		if len(stage.Attempts) >= 32 {
			return "", "", nil, fmt.Errorf("%w: stage attempt limit reached", ErrPlanTransition)
		}
		now := time.Now().UTC()
		stage.Status = PlanStageStatusRunning
		stage.Attempts = append(stage.Attempts, PlanStageAttempt{
			RunID: req.RunID, PlanRevision: plan.Revision,
			Status: PlanStageAttemptRunning, StartedAt: now,
		})
		plan.Status = PlanStatusExecuting
		return "plan_stage_started", "Plan stage started", map[string]string{
			"planId": plan.ID, "stageId": req.StageID, "runId": req.RunID,
		}, nil
	})
}

// FinishPlanStage commits an attempt result. Completion requires a passing
// server-recorded receipt and an explicit complete acceptance snapshot.
func (s *Store) FinishPlanStage(workstreamID, planID string, req FinishPlanStageRequest) (*Plan, error) {
	if req.Status != PlanStageStatusCompleted && req.Status != PlanStageStatusFailed {
		return nil, fmt.Errorf("%w: result status", ErrPlanInvalid)
	}
	return s.mutatePlan(workstreamID, planID, req.ExpectedRevision, req.ExpectedStateRevision, func(plan *Plan) (string, string, map[string]string, error) {
		index := planStageIndex(plan.Stages, req.StageID)
		if index < 0 {
			return "", "", nil, fmt.Errorf("%w: stage does not belong to plan", ErrPlanTransition)
		}
		stage := &plan.Stages[index]
		if plan.Status != PlanStatusExecuting || stage.Status != PlanStageStatusRunning || len(stage.Attempts) == 0 {
			return "", "", nil, fmt.Errorf("%w: stage has no active attempt", ErrPlanTransition)
		}
		attempt := &stage.Attempts[len(stage.Attempts)-1]
		if attempt.RunID != req.RunID || attempt.PlanRevision != plan.Revision || attempt.Status != PlanStageAttemptRunning {
			return "", "", nil, fmt.Errorf("%w: result does not match active attempt", ErrPlanTransition)
		}
		if req.Status == PlanStageStatusCompleted {
			if req.VerificationStatus != "passed" || req.CompletionStatus != "complete" ||
				!validDigest(req.ReceiptDigest) || !validDigest(req.CriteriaDigest) ||
				(req.EvidenceSource != "agent-receipt" && req.EvidenceSource != "team-receipt") {
				return "", "", nil, fmt.Errorf("%w: completed stage requires verified receipt evidence", ErrPlanEvidence)
			}
			now := time.Now().UTC()
			attempt.Status = PlanStageAttemptComplete
			attempt.FinishedAt = now
			attempt.VerificationStatus = req.VerificationStatus
			attempt.ReceiptDigest = req.ReceiptDigest
			attempt.Evidence = &PlanAcceptanceEvidence{
				Source: req.EvidenceSource, ReceiptDigest: req.ReceiptDigest,
				CriteriaDigest: req.CriteriaDigest, VerificationStatus: req.VerificationStatus,
				CompletionStatus: req.CompletionStatus, RecordedAt: now,
			}
			stage.Status = PlanStageStatusCompleted
			allComplete := true
			for _, candidate := range plan.Stages {
				if candidate.Status != PlanStageStatusCompleted {
					allComplete = false
					break
				}
			}
			if allComplete {
				plan.Status = PlanStatusCompleted
			}
			return "plan_stage_completed", "Plan stage completed with verified evidence", map[string]string{
				"planId": plan.ID, "stageId": req.StageID, "runId": req.RunID,
			}, nil
		}
		verification := req.VerificationStatus
		if verification == "" {
			verification = "not-run"
		}
		if verification != "passed" && verification != "failed" && verification != "not-run" {
			return "", "", nil, fmt.Errorf("%w: verification status", ErrPlanInvalid)
		}
		if req.ReceiptDigest != "" && !validDigest(req.ReceiptDigest) {
			return "", "", nil, fmt.Errorf("%w: receipt digest", ErrPlanEvidence)
		}
		attempt.Status = PlanStageAttemptFailed
		attempt.FinishedAt = time.Now().UTC()
		attempt.VerificationStatus = verification
		attempt.ReceiptDigest = req.ReceiptDigest
		attempt.Evidence = nil
		stage.Status = PlanStageStatusFailed
		plan.Status = PlanStatusFailed
		return "plan_stage_failed", "Plan stage failed verification or execution", map[string]string{
			"planId": plan.ID, "stageId": req.StageID, "runId": req.RunID, "verification": verification,
		}, nil
	})
}

// ReconcilePlanStage closes an interrupted attempt as incomplete only after an
// explicit operator acknowledgement. It never creates acceptance evidence.
func (s *Store) ReconcilePlanStage(workstreamID, planID string, req ReconcilePlanStageRequest) (*Plan, error) {
	if !req.ManualAcknowledged {
		return nil, fmt.Errorf("%w: manual stopped-run acknowledgement is required", ErrPlanTransition)
	}
	if !validPlanRunID(req.RunID) || !validPlanID(req.StageID) {
		return nil, fmt.Errorf("%w: stage or run id", ErrPlanInvalid)
	}
	return s.mutatePlan(workstreamID, planID, req.ExpectedRevision, req.ExpectedStateRevision, func(plan *Plan) (string, string, map[string]string, error) {
		index := planStageIndex(plan.Stages, req.StageID)
		if index < 0 {
			return "", "", nil, fmt.Errorf("%w: stage does not belong to plan", ErrPlanTransition)
		}
		stage := &plan.Stages[index]
		if plan.Status != PlanStatusExecuting || stage.Status != PlanStageStatusRunning || len(stage.Attempts) == 0 {
			return "", "", nil, fmt.Errorf("%w: stage has no interrupted attempt", ErrPlanTransition)
		}
		attempt := &stage.Attempts[len(stage.Attempts)-1]
		if attempt.RunID != req.RunID || attempt.PlanRevision != plan.Revision || attempt.Status != PlanStageAttemptRunning {
			return "", "", nil, fmt.Errorf("%w: reconciliation does not match active attempt", ErrPlanTransition)
		}
		attempt.Status = PlanStageAttemptFailed
		attempt.FinishedAt = time.Now().UTC()
		attempt.VerificationStatus = "not-run"
		attempt.Evidence = nil
		stage.Status = PlanStageStatusFailed
		plan.Status = PlanStatusFailed
		return "plan_stage_reconciled", "Interrupted Plan stage reconciled as incomplete", map[string]string{
			"planId": plan.ID, "stageId": req.StageID, "runId": req.RunID,
			"reconciliation": "manual", "verification": "not-run",
		}, nil
	})
}

func (s *Store) mutatePlan(workstreamID, planID string, expectedRevision, expectedStateRevision uint64, change func(*Plan) (string, string, map[string]string, error)) (*Plan, error) {
	if s == nil {
		return nil, fmt.Errorf("workstream: store is unavailable")
	}
	workstreamID = strings.TrimSpace(workstreamID)
	if _, err := s.Get(workstreamID); err != nil {
		return nil, err
	}
	if !validPlanID(planID) {
		return nil, fmt.Errorf("%w: plan id", ErrPlanInvalid)
	}
	if expectedRevision == 0 || expectedStateRevision == 0 {
		return nil, fmt.Errorf("%w: expected revisions are required", ErrPlanRevisionConflict)
	}
	lock := planStoreLock(s.workspace, workstreamID)
	lock.Lock()
	defer lock.Unlock()
	plan, err := s.readPlanLocked(workstreamID, planID)
	if err != nil {
		return nil, err
	}
	if plan.Revision != expectedRevision || plan.StateRevision != expectedStateRevision {
		return nil, fmt.Errorf("%w: stale plan state", ErrPlanRevisionConflict)
	}
	if plan.StateRevision == ^uint64(0) {
		return nil, fmt.Errorf("%w: state revision exhausted", ErrPlanRevisionConflict)
	}
	eventType, eventMessage, eventData, err := change(plan)
	if err != nil {
		return nil, err
	}
	plan.StateRevision++
	plan.UpdatedAt = time.Now().UTC()
	if err := validatePlanRecord(*plan, workstreamID, planID); err != nil {
		return nil, err
	}
	if err := writeJSONAtomic(PlanPath(s.workspace, workstreamID, planID), plan); err != nil {
		return nil, err
	}
	if eventData == nil {
		eventData = make(map[string]string)
	}
	eventData["planId"] = plan.ID
	eventData["revision"] = fmt.Sprint(plan.Revision)
	eventData["stateRevision"] = fmt.Sprint(plan.StateRevision)
	s.recordPlanEvent(workstreamID, eventType, eventMessage, eventData)
	return clonePlan(plan), nil
}

func (s *Store) recordPlanEvent(workstreamID, eventType, message string, data map[string]string) {
	s.mu.Lock()
	err := s.appendEventLocked(workstreamID, TimelineEvent{Type: eventType, Message: message, Data: data})
	s.mu.Unlock()
	if err != nil {
		// The plan file is already the canonical state. Do not report a failed
		// transition after its atomic write has committed.
		log.Printf("[workstream] plan event %s was not recorded: %v", eventType, err)
	}
}

func hasPlanStageAttempts(stages []PlanStage) bool {
	for _, stage := range stages {
		if len(stage.Attempts) != 0 {
			return true
		}
	}
	return false
}

func planHasRunningStage(stages []PlanStage) bool {
	for _, stage := range stages {
		if stage.Status == PlanStageStatusRunning {
			return true
		}
	}
	return false
}

func planStageIndex(stages []PlanStage, stageID string) int {
	for index, stage := range stages {
		if stage.ID == stageID {
			return index
		}
	}
	return -1
}

func validPlanRunID(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\\/\x00\t\r\n")
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func (s *Store) readPlanLocked(workstreamID, planID string) (*Plan, error) {
	if !validPlanID(planID) {
		return nil, fmt.Errorf("%w: plan id", ErrPlanInvalid)
	}
	path := PlanPath(s.workspace, workstreamID, planID)
	var plan Plan
	if err := readJSON(path, &plan); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrPlanNotFound, planID)
		}
		return nil, fmt.Errorf("%w: read stored plan", ErrPlanCorrupt)
	}
	if err := validatePlanRecord(plan, workstreamID, planID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPlanCorrupt, err)
	}
	return clonePlan(&plan), nil
}

func validatePlanRecord(plan Plan, workstreamID, planID string) error {
	if plan.Version != CurrentPlanVersion || plan.ID != planID || plan.WorkstreamID != workstreamID ||
		!validPlanID(plan.ID) || plan.Revision == 0 || plan.StateRevision == 0 ||
		plan.CreatedAt.IsZero() || plan.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: identity or revision", ErrPlanInvalid)
	}
	switch plan.Status {
	case PlanStatusDraft, PlanStatusApproved, PlanStatusExecuting, PlanStatusCompleted, PlanStatusFailed:
	default:
		return fmt.Errorf("%w: status", ErrPlanInvalid)
	}
	if plan.ApprovedRevision > plan.Revision ||
		(plan.Status == PlanStatusDraft && plan.ApprovedRevision != 0) ||
		(plan.Status != PlanStatusDraft && plan.ApprovedRevision != plan.Revision) {
		return fmt.Errorf("%w: approval revision", ErrPlanInvalid)
	}
	definitionStageIDs, err := planDefinitionStageIDs(plan.Definition)
	if err != nil {
		return err
	}
	if len(definitionStageIDs) != len(plan.Stages) {
		return fmt.Errorf("%w: stage state does not match definition", ErrPlanInvalid)
	}
	seenRunIDs := make(map[string]struct{})
	runningAttempts := 0
	for index, stage := range plan.Stages {
		if stage.ID != definitionStageIDs[index] || !validPlanID(stage.ID) {
			return fmt.Errorf("%w: stage identity", ErrPlanInvalid)
		}
		switch stage.Status {
		case PlanStageStatusPending, PlanStageStatusRunning, PlanStageStatusCompleted, PlanStageStatusFailed:
		default:
			return fmt.Errorf("%w: stage status", ErrPlanInvalid)
		}
		if len(stage.Attempts) > 32 {
			return fmt.Errorf("%w: too many stage attempts", ErrPlanInvalid)
		}
		for attemptIndex, attempt := range stage.Attempts {
			if !validPlanRunID(attempt.RunID) || attempt.PlanRevision != plan.Revision || attempt.StartedAt.IsZero() {
				return fmt.Errorf("%w: stage attempt identity", ErrPlanInvalid)
			}
			if _, exists := seenRunIDs[attempt.RunID]; exists {
				return fmt.Errorf("%w: duplicate stage attempt run id", ErrPlanInvalid)
			}
			seenRunIDs[attempt.RunID] = struct{}{}
			if attemptIndex < len(stage.Attempts)-1 && attempt.Status != PlanStageAttemptFailed {
				return fmt.Errorf("%w: non-final attempt must be failed", ErrPlanInvalid)
			}
			switch attempt.Status {
			case PlanStageAttemptRunning:
				runningAttempts++
				if attemptIndex != len(stage.Attempts)-1 || !attempt.FinishedAt.IsZero() || attempt.Evidence != nil {
					return fmt.Errorf("%w: running attempt state", ErrPlanInvalid)
				}
			case PlanStageAttemptComplete:
				if attemptIndex != len(stage.Attempts)-1 || attempt.FinishedAt.IsZero() || attempt.Evidence == nil ||
					attempt.VerificationStatus != "passed" || attempt.ReceiptDigest != attempt.Evidence.ReceiptDigest ||
					!validAcceptanceEvidence(*attempt.Evidence) {
					return fmt.Errorf("%w: completed attempt evidence", ErrPlanInvalid)
				}
			case PlanStageAttemptFailed:
				if attempt.FinishedAt.IsZero() || attempt.Evidence != nil ||
					(attempt.VerificationStatus != "" && attempt.VerificationStatus != "passed" && attempt.VerificationStatus != "failed" && attempt.VerificationStatus != "not-run") ||
					(attempt.ReceiptDigest != "" && !validDigest(attempt.ReceiptDigest)) {
					return fmt.Errorf("%w: failed attempt state", ErrPlanInvalid)
				}
			default:
				return fmt.Errorf("%w: stage attempt status", ErrPlanInvalid)
			}
		}
		if (stage.Status == PlanStageStatusPending && len(stage.Attempts) != 0) ||
			(stage.Status == PlanStageStatusRunning && (len(stage.Attempts) == 0 || stage.Attempts[len(stage.Attempts)-1].Status != PlanStageAttemptRunning)) ||
			(stage.Status == PlanStageStatusCompleted && (len(stage.Attempts) == 0 || stage.Attempts[len(stage.Attempts)-1].Status != PlanStageAttemptComplete)) ||
			(stage.Status == PlanStageStatusFailed && (len(stage.Attempts) == 0 || stage.Attempts[len(stage.Attempts)-1].Status != PlanStageAttemptFailed)) {
			return fmt.Errorf("%w: stage attempt does not match stage state", ErrPlanInvalid)
		}
	}
	if runningAttempts > 1 {
		return fmt.Errorf("%w: multiple stage attempts are running", ErrPlanInvalid)
	}
	if plan.Status == PlanStatusDraft || plan.Status == PlanStatusApproved {
		if hasPlanStageAttempts(plan.Stages) || planHasAnyStageState(plan.Stages, PlanStageStatusRunning, PlanStageStatusCompleted, PlanStageStatusFailed) {
			return fmt.Errorf("%w: unexecuted plan has progress", ErrPlanInvalid)
		}
	}
	if plan.Status == PlanStatusExecuting && planHasAnyStageState(plan.Stages, PlanStageStatusFailed) {
		return fmt.Errorf("%w: executing plan has failed stage", ErrPlanInvalid)
	}
	if plan.Status == PlanStatusFailed && (planHasRunningStage(plan.Stages) || !planHasAnyStageState(plan.Stages, PlanStageStatusFailed)) {
		return fmt.Errorf("%w: failed plan stage state", ErrPlanInvalid)
	}
	if plan.Status == PlanStatusCompleted {
		for _, stage := range plan.Stages {
			if stage.Status != PlanStageStatusCompleted {
				return fmt.Errorf("%w: completed plan has incomplete stage", ErrPlanInvalid)
			}
		}
	}
	return nil
}

func planHasAnyStageState(stages []PlanStage, statuses ...string) bool {
	for _, stage := range stages {
		for _, status := range statuses {
			if stage.Status == status {
				return true
			}
		}
	}
	return false
}

func validAcceptanceEvidence(evidence PlanAcceptanceEvidence) bool {
	return (evidence.Source == "agent-receipt" || evidence.Source == "team-receipt") &&
		validDigest(evidence.ReceiptDigest) && validDigest(evidence.CriteriaDigest) &&
		evidence.VerificationStatus == "passed" && evidence.CompletionStatus == "complete" &&
		!evidence.RecordedAt.IsZero()
}

func planDefinitionStageIDs(definition json.RawMessage) ([]string, error) {
	if len(definition) == 0 || len(definition) > maxPlanDefinitionBytes || !json.Valid(definition) {
		return nil, fmt.Errorf("%w: definition is missing, malformed, or too large", ErrPlanInvalid)
	}
	var envelope struct {
		Stages []struct {
			ID string `json:"id"`
		} `json:"stages"`
	}
	if err := json.Unmarshal(definition, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decode stages: %v", ErrPlanInvalid, err)
	}
	seen := make(map[string]struct{}, len(envelope.Stages))
	ids := make([]string, 0, len(envelope.Stages))
	for _, stage := range envelope.Stages {
		id := strings.TrimSpace(stage.ID)
		if !validPlanID(id) || id != stage.ID {
			return nil, fmt.Errorf("%w: stage id", ErrPlanInvalid)
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("%w: duplicate stage id", ErrPlanInvalid)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func validPlanID(id string) bool {
	return id != "" && len(id) <= 256 && strings.TrimSpace(id) == id &&
		id != "." && id != ".." && sanitizeID(id) == id &&
		!strings.ContainsAny(id, "/\\\x00\t\r\n")
}

func planStoreLock(workspace, workstreamID string) *sync.Mutex {
	key := filepath.Clean(PlansDir(workspace, workstreamID))
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	lock, _ := planStoreLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func newPlanID(now time.Time, definition json.RawMessage) string {
	var envelope struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(definition, &envelope)
	name := slug(envelope.Name)
	if name == "" {
		name = "plan"
	}
	return "plan_" + now.Format("20060102T150405.000000000") + "_" + name
}

func clonePlan(plan *Plan) *Plan {
	if plan == nil {
		return nil
	}
	copyPlan := *plan
	copyPlan.Definition = append(json.RawMessage(nil), plan.Definition...)
	copyPlan.Stages = append([]PlanStage(nil), plan.Stages...)
	for index := range copyPlan.Stages {
		copyPlan.Stages[index].Attempts = append([]PlanStageAttempt(nil), plan.Stages[index].Attempts...)
		for attemptIndex := range copyPlan.Stages[index].Attempts {
			evidence := plan.Stages[index].Attempts[attemptIndex].Evidence
			if evidence != nil {
				copyEvidence := *evidence
				copyPlan.Stages[index].Attempts[attemptIndex].Evidence = &copyEvidence
			}
		}
	}
	return &copyPlan
}
