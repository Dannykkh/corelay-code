package workstream

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	workspace string
	mu        sync.Mutex
}

func NewStore(workspace string) *Store {
	return &Store{workspace: filepath.Clean(workspace)}
}

func (s *Store) Workspace() string {
	return s.workspace
}

func (s *Store) Create(req CreateRequest) (*Workstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, fmt.Errorf("workstream: title is required")
	}
	now := time.Now().UTC()
	id := sanitizeID(req.ID)
	if req.ID == "" {
		id = newID(now, title)
	}
	if id != sanitizeID(id) {
		return nil, fmt.Errorf("workstream: invalid id")
	}
	path := StatePath(s.workspace, id)
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("workstream: id already exists: %s", id)
	}

	ws := &Workstream{
		Version:    CurrentVersion,
		ID:         id,
		Title:      title,
		Workspace:  s.workspace,
		Status:     StatusActive,
		Summary:    strings.TrimSpace(req.Summary),
		NextAction: strings.TrimSpace(req.NextAction),
		Tags:       append([]string(nil), req.Tags...),
		Goal:       req.Goal,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if ws.Goal.VerificationPolicy.MaxRepairAttempts == 0 {
		ws.Goal.VerificationPolicy.MaxRepairAttempts = 2
	}
	if err := writeJSONAtomic(path, ws); err != nil {
		return nil, err
	}
	if err := s.appendEventLocked(id, TimelineEvent{
		Type:    "created",
		Message: "Workstream created",
		Data:    map[string]string{"title": title},
	}); err != nil {
		return nil, err
	}
	return ws, nil
}

func (s *Store) Get(id string) (*Workstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(id)
}

func (s *Store) List(status Status) ([]Workstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(Root(s.workspace))
	if os.IsNotExist(err) {
		return []Workstream{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workstream: list: %w", err)
	}
	var out []Workstream
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var ws Workstream
		if err := readJSON(filepath.Join(Root(s.workspace), e.Name(), workstreamJSONName), &ws); err != nil {
			continue
		}
		if status != "" && ws.Status != status {
			continue
		}
		out = append(out, ws)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

func (s *Store) Patch(id string, patch Patch) (*Workstream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ws, err := s.getLocked(id)
	if err != nil {
		return nil, err
	}
	changed := map[string]string{}
	if patch.Status != nil {
		if !patch.Status.Valid() {
			return nil, fmt.Errorf("workstream: invalid status %q", *patch.Status)
		}
		ws.Status = *patch.Status
		changed["status"] = string(*patch.Status)
	}
	if patch.Summary != nil {
		ws.Summary = strings.TrimSpace(*patch.Summary)
		changed["summary"] = ws.Summary
	}
	if patch.NextAction != nil {
		ws.NextAction = strings.TrimSpace(*patch.NextAction)
		changed["nextAction"] = ws.NextAction
	}
	if patch.OpenQuestions != nil {
		ws.OpenQuestions = append([]string(nil), patch.OpenQuestions...)
		changed["openQuestions"] = fmt.Sprintf("%d", len(ws.OpenQuestions))
	}
	if patch.Tags != nil {
		ws.Tags = append([]string(nil), patch.Tags...)
		changed["tags"] = strings.Join(ws.Tags, ",")
	}
	if patch.Goal != nil {
		ws.Goal = *patch.Goal
		changed["goal"] = "updated"
	}
	if patch.LastVerification != nil {
		ws.LastVerification = *patch.LastVerification
		if ws.LastVerification.UpdatedAt.IsZero() {
			ws.LastVerification.UpdatedAt = time.Now().UTC()
		}
		changed["verification"] = ws.LastVerification.Status
	}
	ws.UpdatedAt = time.Now().UTC()
	if err := writeJSONAtomic(StatePath(s.workspace, ws.ID), ws); err != nil {
		return nil, err
	}
	eventType := "updated"
	if _, ok := changed["verification"]; ok {
		eventType = "verification_updated"
	}
	if err := s.appendEventLocked(ws.ID, TimelineEvent{
		Type:    eventType,
		Message: "Workstream updated",
		Data:    changed,
	}); err != nil {
		return nil, err
	}
	return ws, nil
}

func (s *Store) AppendEvent(id string, event TimelineEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.getLocked(id); err != nil {
		return err
	}
	return s.appendEventLocked(id, event)
}

func (s *Store) Timeline(id string) ([]TimelineEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.getLocked(id); err != nil {
		return nil, err
	}
	return readJSONLines[TimelineEvent](TimelinePath(s.workspace, id))
}

func (s *Store) getLocked(id string) (*Workstream, error) {
	cleanID := sanitizeID(id)
	if cleanID == "" || cleanID != id {
		return nil, fmt.Errorf("workstream: invalid id")
	}
	var ws Workstream
	if err := readJSON(StatePath(s.workspace, cleanID), &ws); err != nil {
		return nil, err
	}
	if ws.Workspace != s.workspace {
		return nil, fmt.Errorf("workstream: workspace mismatch")
	}
	return &ws, nil
}

func (s *Store) appendEventLocked(id string, event TimelineEvent) error {
	now := time.Now().UTC()
	if event.Version == 0 {
		event.Version = CurrentVersion
	}
	if event.At.IsZero() {
		event.At = now
	}
	if event.ID == "" {
		event.ID = "evt_" + event.At.Format("20060102T150405.000000000")
	}
	return appendJSONLine(TimelinePath(s.workspace, id), event)
}

func newID(now time.Time, title string) string {
	return "ws_" + now.Format("20060102T150405") + "_" + slug(title)
}
