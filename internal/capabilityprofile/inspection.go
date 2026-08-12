package capabilityprofile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProfileStatus is a bounded exact-target inventory row. It excludes raw
// observations, endpoints, credentials, prompts, and manual-override text.
type ProfileStatus struct {
	ProfileID             string             `json:"profileId"`
	CreatedAt             time.Time          `json:"createdAt"`
	ExpiresAt             time.Time          `json:"expiresAt"`
	Verified              bool               `json:"verified"`
	ConfidenceBasisPoints int                `json:"confidenceBasisPoints"`
	QuarantineReasons     []QuarantineReason `json:"quarantineReasons"`
	Attempts              int                `json:"attempts"`
	ManualOnly            bool               `json:"manualOnly"`
}

// List returns immutable profiles for one exact target, newest first. A
// malformed or unsafe entry fails the inventory rather than being silently
// presented as trustworthy status.
func (s *Store) List(target TargetIdentity) ([]ProfileStatus, error) {
	if s == nil || s.root == "" {
		return nil, ErrUnsafeStorePath
	}
	if !target.Valid() {
		return nil, ErrInvalidTarget
	}
	dir, err := s.targetDirectory(target, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []ProfileStatus{}, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read capability profile inventory: %w", err)
	}
	statuses := make([]ProfileStatus, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		profileID := strings.TrimSuffix(name, ".json")
		if !validDigest(profileID) || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil, ErrUnsafeStorePath
		}
		profile, err := loadProfileFile(filepath.Join(dir, name), target, profileID)
		if err != nil {
			return nil, err
		}
		snapshot := profile.Snapshot()
		statuses = append(statuses, ProfileStatus{
			ProfileID: snapshot.ProfileID, CreatedAt: snapshot.CreatedAt,
			ExpiresAt: snapshot.ExpiresAt, Verified: snapshot.Verified,
			ConfidenceBasisPoints: snapshot.ConfidenceBasisPoints,
			QuarantineReasons:     append([]QuarantineReason(nil), snapshot.QuarantineReasons...),
			Attempts:              len(snapshot.Observations), ManualOnly: len(snapshot.ManualOverrides) != 0,
		})
	}
	sort.Slice(statuses, func(i, j int) bool {
		if !statuses[i].CreatedAt.Equal(statuses[j].CreatedAt) {
			return statuses[i].CreatedAt.After(statuses[j].CreatedAt)
		}
		return statuses[i].ProfileID < statuses[j].ProfileID
	})
	return statuses, nil
}
