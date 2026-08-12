package capabilityprofile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const maxProfileFileBytes = 4 << 20
const maxProfileJSONDepth = 32

type ProfileRef struct {
	TargetDigest string
	ProfileID    string
}

// SelectionError retains only typed reasons. It never includes file content,
// target endpoint, credentials, provider error text, or manual-override text.
type SelectionError struct {
	Reasons []QuarantineReason
}

func (e *SelectionError) Error() string {
	if e == nil || len(e.Reasons) == 0 {
		return ErrNoSelectableProfile.Error()
	}
	parts := make([]string, len(e.Reasons))
	for i, reason := range e.Reasons {
		parts[i] = string(reason)
	}
	return ErrNoSelectableProfile.Error() + ": " + strings.Join(parts, ",")
}

func (e *SelectionError) Unwrap() error { return ErrNoSelectableProfile }

type Store struct {
	root string
}

func NewStore(root string) (*Store, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("%w: empty root", ErrUnsafeStorePath)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid root", ErrUnsafeStorePath)
	}
	absolute = filepath.Clean(absolute)
	if err := ensureSecureDirectory(absolute, true); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("%w: root cannot be resolved", ErrUnsafeStorePath)
	}
	return &Store{root: canonical}, nil
}

func (s *Store) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Save publishes a profile with an atomic hard-link operation. The final path
// is created only if absent, so immutable content is never overwritten even
// when writers race.
func (s *Store) Save(profile CapabilityProfile) (ProfileRef, error) {
	if s == nil || s.root == "" || !profile.Valid() {
		return ProfileRef{}, ErrInvalidProfile
	}
	snapshot := profile.Snapshot()
	target, err := targetFromSnapshot(snapshot.Target)
	if err != nil {
		return ProfileRef{}, err
	}
	dir, err := s.targetDirectory(target, true)
	if err != nil {
		return ProfileRef{}, err
	}
	path := filepath.Join(dir, snapshot.ProfileID+".json")
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return ProfileRef{}, ErrUnsafeStorePath
		}
		return ProfileRef{}, ErrProfileConflict
	} else if !os.IsNotExist(statErr) {
		return ProfileRef{}, fmt.Errorf("inspect immutable profile target: %w", statErr)
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return ProfileRef{}, fmt.Errorf("encode capability profile: %w", err)
	}
	encoded = append(encoded, '\n')
	temp, err := os.CreateTemp(dir, ".capability-profile-*.tmp")
	if err != nil {
		return ProfileRef{}, fmt.Errorf("create capability profile staging file: %w", err)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	resolvedTempParent, resolveErr := filepath.EvalSymlinks(filepath.Dir(tempPath))
	if resolveErr != nil || !samePath(resolvedTempParent, dir) {
		_ = temp.Close()
		return ProfileRef{}, ErrUnsafeStorePath
	}
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return ProfileRef{}, fmt.Errorf("secure capability profile staging file: %w", err)
	}
	if _, err := temp.Write(encoded); err != nil {
		_ = temp.Close()
		return ProfileRef{}, fmt.Errorf("write capability profile staging file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return ProfileRef{}, fmt.Errorf("sync capability profile staging file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return ProfileRef{}, fmt.Errorf("close capability profile staging file: %w", err)
	}
	resolvedDir, resolveErr := filepath.EvalSymlinks(dir)
	if resolveErr != nil || !samePath(resolvedDir, dir) {
		return ProfileRef{}, ErrUnsafeStorePath
	}
	if err := os.Link(tempPath, path); err != nil {
		if os.IsExist(err) {
			return ProfileRef{}, ErrProfileConflict
		}
		return ProfileRef{}, fmt.Errorf("publish immutable capability profile: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return ProfileRef{}, fmt.Errorf("remove capability profile staging link: %w", err)
	}
	removeTemp = false
	if err := syncDirectory(dir); err != nil {
		return ProfileRef{}, err
	}
	return ProfileRef{TargetDigest: target.Digest(), ProfileID: snapshot.ProfileID}, nil
}

func (s *Store) Load(target TargetIdentity, profileID string) (CapabilityProfile, error) {
	if s == nil || s.root == "" {
		return CapabilityProfile{}, ErrUnsafeStorePath
	}
	if !target.Valid() {
		return CapabilityProfile{}, ErrInvalidTarget
	}
	if !validDigest(profileID) {
		return CapabilityProfile{}, ErrProfileNotFound
	}
	dir, err := s.targetDirectory(target, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CapabilityProfile{}, ErrProfileNotFound
		}
		return CapabilityProfile{}, err
	}
	path := filepath.Join(dir, profileID+".json")
	return loadProfileFile(path, target, profileID)
}

// AutoSelect returns only an exact-target, naturally verified, unexpired,
// nonquarantined profile with no manual override records.
func (s *Store) AutoSelect(target TargetIdentity, now time.Time) (CapabilityProfile, error) {
	if s == nil || s.root == "" {
		return CapabilityProfile{}, ErrUnsafeStorePath
	}
	if !target.Valid() {
		return CapabilityProfile{}, ErrInvalidTarget
	}
	dir, err := s.targetDirectory(target, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CapabilityProfile{}, &SelectionError{}
		}
		return CapabilityProfile{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return CapabilityProfile{}, fmt.Errorf("read capability profile target directory: %w", err)
	}
	now = normalizeTime(now)
	candidates := make([]CapabilityProfile, 0)
	reasons := make([]QuarantineReason, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		profileID := strings.TrimSuffix(name, ".json")
		if !validDigest(profileID) || entry.Type()&os.ModeSymlink != 0 {
			reasons = append(reasons, QuarantineCorrupt)
			continue
		}
		profile, loadErr := loadProfileFile(filepath.Join(dir, name), target, profileID)
		if loadErr != nil {
			reasons = append(reasons, reasonForLoadError(loadErr))
			continue
		}
		snapshot := profile.snapshot
		if snapshot.CreatedAt.After(now) {
			reasons = append(reasons, QuarantineCorrupt)
			continue
		}
		if !snapshot.ExpiresAt.After(now) {
			reasons = append(reasons, QuarantineExpired)
			continue
		}
		if len(snapshot.ManualOverrides) != 0 {
			reasons = append(reasons, QuarantineManualOnly)
			continue
		}
		if !snapshot.Verified || len(snapshot.QuarantineReasons) != 0 {
			reasons = append(reasons, snapshot.QuarantineReasons...)
			continue
		}
		candidates = append(candidates, profile)
	}
	if len(candidates) == 0 {
		return CapabilityProfile{}, &SelectionError{Reasons: normalizeReasons(reasons)}
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i].snapshot, candidates[j].snapshot
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.After(right.CreatedAt)
		}
		return left.ProfileID < right.ProfileID
	})
	return candidates[0], nil
}

// SelectManual requires both immutable profile ID and override ID. It never
// participates in automatic selection and cannot revive an expired profile.
func (s *Store) SelectManual(target TargetIdentity, profileID, overrideID string, now time.Time) (CapabilityProfile, error) {
	if !safeIdentifier.MatchString(strings.TrimSpace(overrideID)) {
		return CapabilityProfile{}, ErrManualOverride
	}
	profile, err := s.Load(target, profileID)
	if err != nil {
		return CapabilityProfile{}, err
	}
	now = normalizeTime(now)
	if profile.snapshot.CreatedAt.After(now) || !profile.snapshot.ExpiresAt.After(now) {
		return CapabilityProfile{}, ErrManualOverride
	}
	for _, override := range profile.snapshot.ManualOverrides {
		if override.ID == overrideID && override.ExpiresAt.After(now) {
			return profile, nil
		}
	}
	return CapabilityProfile{}, ErrManualOverride
}

func (s *Store) targetDirectory(target TargetIdentity, create bool) (string, error) {
	if err := ensureSecureDirectory(s.root, false); err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, target.Digest())
	relative, err := filepath.Rel(s.root, dir)
	if err != nil || relative != target.Digest() {
		return "", ErrUnsafeStorePath
	}
	if err := ensureSecureDirectory(dir, create); err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	relativeCanonical, err := filepath.Rel(s.root, canonical)
	if err != nil || relativeCanonical != target.Digest() {
		return "", ErrUnsafeStorePath
	}
	return canonical, nil
}

func loadProfileFile(path string, target TargetIdentity, profileID string) (CapabilityProfile, error) {
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || !samePath(resolvedParent, filepath.Dir(path)) {
		return CapabilityProfile{}, ErrUnsafeStorePath
	}
	lexical, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return CapabilityProfile{}, ErrProfileNotFound
		}
		return CapabilityProfile{}, fmt.Errorf("inspect capability profile: %w", err)
	}
	if lexical.Mode()&os.ModeSymlink != 0 || !lexical.Mode().IsRegular() || lexical.Size() <= 0 || lexical.Size() > maxProfileFileBytes {
		return CapabilityProfile{}, ErrUnsafeStorePath
	}
	file, err := os.Open(path)
	if err != nil {
		return CapabilityProfile{}, fmt.Errorf("open capability profile: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(lexical, opened) {
		return CapabilityProfile{}, ErrUnsafeStorePath
	}
	data, err := io.ReadAll(io.LimitReader(file, maxProfileFileBytes+1))
	if err != nil || len(data) > maxProfileFileBytes {
		return CapabilityProfile{}, ErrInvalidProfile
	}
	if err := validateStrictJSON(data); err != nil {
		return CapabilityProfile{}, ErrInvalidProfile
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var snapshot ProfileSnapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return CapabilityProfile{}, classifyDecodeError(snapshot.SchemaVersion)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return CapabilityProfile{}, ErrInvalidProfile
	}
	if snapshot.SchemaVersion != CurrentProfileSchemaVersion {
		return CapabilityProfile{}, ErrSchemaMismatch
	}
	if snapshot.ProfileID != profileID {
		return CapabilityProfile{}, ErrInvalidProfile
	}
	if snapshot.Target.TargetDigest != target.Digest() {
		return CapabilityProfile{}, ErrTargetMismatch
	}
	profile, err := profileFromSnapshot(snapshot, false)
	if err != nil {
		return CapabilityProfile{}, err
	}
	return profile, nil
}

func classifyDecodeError(schemaVersion int) error {
	if schemaVersion != 0 && schemaVersion != CurrentProfileSchemaVersion {
		return ErrSchemaMismatch
	}
	return ErrInvalidProfile
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrInvalidProfile
		}
		return err
	}
	return nil
}

func validateStrictJSON(data []byte) error {
	if !utf8.Valid(data) {
		return ErrInvalidProfile
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxProfileJSONDepth {
		return ErrInvalidProfile
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrInvalidProfile
			}
			canonicalKey := strings.ToLower(key)
			if _, exists := keys[canonicalKey]; exists {
				return ErrInvalidProfile
			}
			keys[canonicalKey] = struct{}{}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return ErrInvalidProfile
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return ErrInvalidProfile
		}
	default:
		return ErrInvalidProfile
	}
	return nil
}

func reasonForLoadError(err error) QuarantineReason {
	switch {
	case errors.Is(err, ErrSchemaMismatch):
		return QuarantineSchemaMismatch
	case errors.Is(err, ErrTargetMismatch):
		return QuarantineTargetChanged
	default:
		return QuarantineCorrupt
	}
}

func ensureSecureDirectory(path string, create bool) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && create {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create capability profile directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return ErrUnsafeStorePath
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure capability profile directory: %w", err)
	}
	if _, err := filepath.EvalSymlinks(path); err != nil {
		return ErrUnsafeStorePath
	}
	return nil
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open capability profile directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync capability profile directory: %w", err)
	}
	return nil
}
