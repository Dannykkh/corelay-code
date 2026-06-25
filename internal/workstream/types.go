package workstream

import "time"

const CurrentVersion = 1

type Status string

const (
	StatusActive    Status = "active"
	StatusBlocked   Status = "blocked"
	StatusCompleted Status = "completed"
	StatusArchived  Status = "archived"
)

func (s Status) Valid() bool {
	switch s {
	case StatusActive, StatusBlocked, StatusCompleted, StatusArchived:
		return true
	default:
		return false
	}
}

type Workstream struct {
	Version          int                `json:"version"`
	ID               string             `json:"id"`
	Title            string             `json:"title"`
	Workspace        string             `json:"workspace"`
	Status           Status             `json:"status"`
	Summary          string             `json:"summary,omitempty"`
	NextAction       string             `json:"nextAction,omitempty"`
	OpenQuestions    []string           `json:"openQuestions,omitempty"`
	Tags             []string           `json:"tags,omitempty"`
	Goal             Goal               `json:"goal"`
	LastVerification VerificationResult `json:"lastVerification,omitempty"`
	CreatedAt        time.Time          `json:"createdAt"`
	UpdatedAt        time.Time          `json:"updatedAt"`
}

type Goal struct {
	Objective          string             `json:"objective"`
	AcceptanceCriteria []string           `json:"acceptanceCriteria,omitempty"`
	Constraints        []string           `json:"constraints,omitempty"`
	DefinitionOfDone   []string           `json:"definitionOfDone,omitempty"`
	VerificationPolicy VerificationPolicy `json:"verificationPolicy,omitempty"`
}

type VerificationPolicy struct {
	Commands          []string `json:"commands,omitempty"`
	RequiredSignals   []string `json:"requiredSignals,omitempty"`
	MaxRepairAttempts int      `json:"maxRepairAttempts,omitempty"`
}

type VerificationResult struct {
	Status    string    `json:"status,omitempty"` // passed, failed, not-run
	Source    string    `json:"source,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

type TimelineEvent struct {
	Version int               `json:"version"`
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	At      time.Time         `json:"at"`
	Message string            `json:"message,omitempty"`
	Data    map[string]string `json:"data,omitempty"`
}

type CreateRequest struct {
	ID         string
	Title      string
	Summary    string
	NextAction string
	Tags       []string
	Goal       Goal
}

type Patch struct {
	Status           *Status
	Summary          *string
	NextAction       *string
	OpenQuestions    []string
	Tags             []string
	Goal             *Goal
	LastVerification *VerificationResult
}

type HandoffOptions struct {
	IncludeReceipts    bool
	IncludeMemoryIndex bool
}

type HandoffSnapshot struct {
	Path     string
	Markdown string
}
