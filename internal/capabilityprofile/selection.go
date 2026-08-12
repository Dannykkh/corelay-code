package capabilityprofile

import (
	"fmt"
	"strings"
	"time"
)

// AutomaticSelection binds one immutable profile to the exact target identity
// used by the composition root. TargetIdentity retains only one-way endpoint
// and serving-parameter digests, so this value can be injected into a run
// without exposing an endpoint or credential to the agent loop.
type AutomaticSelection struct {
	target  TargetIdentity
	profile CapabilityProfile
}

// NewAutomaticSelection validates all automatic-use gates and binds profile to
// target. Manual selections deliberately cannot be converted into this type.
func NewAutomaticSelection(target TargetIdentity, profile CapabilityProfile, now time.Time) (AutomaticSelection, error) {
	if err := automaticSelectionError(target, profile, now); err != nil {
		return AutomaticSelection{}, err
	}
	return AutomaticSelection{target: target, profile: profile}, nil
}

// RecommendationsFor returns a defensive recommendation value only while the
// selection is still current and its public provider/model identity exactly
// matches the run target. Endpoint and serving-parameter matching happened
// when NewAutomaticSelection bound the exact TargetIdentity.
func (s AutomaticSelection) RecommendationsFor(provider, model string, now time.Time) (Recommendations, string, bool) {
	if strings.TrimSpace(provider) != s.target.Provider() || strings.TrimSpace(model) != s.target.Model() {
		return Recommendations{}, "", false
	}
	if err := automaticSelectionError(s.target, s.profile, now); err != nil {
		return Recommendations{}, "", false
	}
	return s.profile.snapshot.Recommendations, s.profile.ID(), true
}

func automaticSelectionError(target TargetIdentity, profile CapabilityProfile, now time.Time) error {
	if !target.Valid() {
		return ErrInvalidTarget
	}
	if !profile.Valid() {
		return &SelectionError{Reasons: []QuarantineReason{QuarantineCorrupt}}
	}
	snapshot := profile.snapshot
	if snapshot.Target.TargetDigest != target.Digest() ||
		snapshot.Target.Provider != target.Provider() ||
		snapshot.Target.Model != target.Model() {
		return fmt.Errorf("%w: automatic selection target changed", ErrTargetMismatch)
	}
	now = normalizeTime(now)
	if snapshot.CreatedAt.After(now) {
		return &SelectionError{Reasons: []QuarantineReason{QuarantineCorrupt}}
	}
	if !snapshot.ExpiresAt.After(now) {
		return &SelectionError{Reasons: []QuarantineReason{QuarantineExpired}}
	}
	if len(snapshot.ManualOverrides) != 0 {
		return &SelectionError{Reasons: []QuarantineReason{QuarantineManualOnly}}
	}
	if !snapshot.Verified || len(snapshot.QuarantineReasons) != 0 {
		reasons := normalizeReasons(snapshot.QuarantineReasons)
		if len(reasons) == 0 {
			reasons = []QuarantineReason{QuarantineCorrupt}
		}
		return &SelectionError{Reasons: reasons}
	}
	return nil
}
