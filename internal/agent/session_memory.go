package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const (
	maxInlineToolResultBytes  = 2_000
	toolResultPreviewBytes    = 500
	maxSessionResultBlobBytes = 64 << 20
	maxSessionResultStoreSize = 256 << 20
	maxSessionResultBlobs     = 512
	maxSessionMemoryIDBytes   = 256
	maxBlobPublishAttempts    = 16
)

var sessionMemoryLocks sync.Map

// SessionMemory stores large tool results as content-addressed blobs. The
// session label is represented only by a SHA-256 directory key, so caller
// input can never become a filesystem path component.
type SessionMemory struct {
	mu      *sync.RWMutex
	root    string
	dir     string
	initErr error
}

// NewSessionMemory preserves the historical constructor. Invalid storage
// configuration produces a fail-closed store: small results remain inline and
// large results are left to the context planner's deterministic reducer.
func NewSessionMemory(baseDir, sessionID string) *SessionMemory {
	store, err := OpenSessionMemory(baseDir, sessionID)
	if err == nil {
		return store
	}
	return &SessionMemory{mu: &sync.RWMutex{}, initErr: err}
}

// OpenSessionMemory creates or opens one bounded result store.
func OpenSessionMemory(baseDir, sessionID string) (*SessionMemory, error) {
	if strings.TrimSpace(baseDir) == "" || !validSessionMemoryLabel(sessionID) {
		return nil, fmt.Errorf("invalid session result store identity")
	}

	base, err := createCanonicalDirectory(baseDir, 0o700)
	if err != nil {
		return nil, fmt.Errorf("open session result base: %w", err)
	}
	root, err := createDirectChildDirectory(base, "session-output", 0o700)
	if err != nil {
		return nil, fmt.Errorf("open session result root: %w", err)
	}
	key := sessionMemoryKey(sessionID)
	dir, err := createDirectChildDirectory(root, key, 0o700)
	if err != nil {
		return nil, fmt.Errorf("open session result directory: %w", err)
	}
	lockKey := canonicalCasePath(dir)
	lock, _ := sessionMemoryLocks.LoadOrStore(lockKey, &sync.RWMutex{})
	return &SessionMemory{
		mu:   lock.(*sync.RWMutex),
		root: root,
		dir:  dir,
	}, nil
}

func validSessionMemoryLabel(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxSessionMemoryIDBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func sessionMemoryKey(sessionID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(sessionID)))
	return "sm_" + hex.EncodeToString(digest[:])
}

// StoreResultChecked saves a large tool result and distinguishes an inline
// result from a failed durable publication. Its error is intentionally generic
// so paths and raw result content cannot cross the provider/event boundary.
func (sm *SessionMemory) StoreResultChecked(toolName string, result string) (string, ToolResultStoreDisposition, error) {
	if len(result) <= maxInlineToolResultBytes {
		return result, ToolResultInline, nil
	}
	if sm == nil || sm.initErr != nil || sm.mu == nil || sm.dir == "" || len(result) > maxSessionResultBlobBytes {
		return "", ToolResultInline, ErrToolResultPersistence
	}

	digest := sha256.Sum256([]byte(result))
	id := hex.EncodeToString(digest[:])
	sm.mu.Lock()
	err := sm.storeBlobLocked(id, result)
	sm.mu.Unlock()
	if err != nil {
		return "", ToolResultInline, ErrToolResultPersistence
	}

	redactedResult := redactSensitiveString(result)
	preview, truncated := boundedUTF8Prefix(redactedResult, toolResultPreviewBytes)
	if truncated {
		preview += "..."
	}
	toolName = sanitizeReceiptString(sanitizeResultToolName(toolName))
	return fmt.Sprintf(
		"[Tool result stored: result_%s (%d bytes, tool=%s); reference=tool-result://sha256:%s]\n%s\n[Load with LoadToolResult id=result_%s]",
		id,
		len(result),
		toolName,
		id,
		preview,
		id,
	), ToolResultPersisted, nil
}

// StoreResult preserves the historical API. New run composition uses the
// checked form so a false result can never make a failed large write look like
// an ordinary inline value.
func (sm *SessionMemory) StoreResult(toolName string, result string) (string, bool) {
	reference, disposition, err := sm.StoreResultChecked(toolName, result)
	if err != nil {
		return result, false
	}
	return reference, disposition == ToolResultPersisted
}

func (sm *SessionMemory) storeBlobLocked(id, result string) error {
	target, err := sm.blobPath(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return verifyResultBlob(target, id)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	count, total, err := sm.usageLocked()
	if err != nil {
		return err
	}
	if count >= maxSessionResultBlobs || total+int64(len(result)) > maxSessionResultStoreSize {
		return fmt.Errorf("session result store quota exceeded")
	}

	temp, err := sm.createBlobTempLocked()
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	if _, err := io.Copy(temp, strings.NewReader(result)); err != nil {
		cleanup()
		return err
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Link(tempPath, target); err != nil {
		_ = os.Remove(tempPath)
		if errors.Is(err, os.ErrExist) {
			return verifyResultBlob(target, id)
		}
		return err
	}
	if err := os.Remove(tempPath); err != nil {
		return err
	}
	if err := syncSessionMemoryDirectory(sm.dir); err != nil {
		return err
	}
	return verifyResultBlob(target, id)
}

func (sm *SessionMemory) createBlobTempLocked() (*os.File, error) {
	for attempt := 0; attempt < maxBlobPublishAttempts; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		path := filepath.Join(sm.dir, ".tmp_"+hex.EncodeToString(random[:]))
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("allocate session result temp file")
}

func (sm *SessionMemory) blobPath(id string) (string, error) {
	id = normalizeResultID(id)
	if !validResultDigest(id) || sm == nil || sm.dir == "" {
		return "", fmt.Errorf("invalid result id")
	}
	path := filepath.Join(sm.dir, "result_"+id+".blob")
	relative, err := filepath.Rel(sm.dir, path)
	if err != nil || relative != "result_"+id+".blob" {
		return "", fmt.Errorf("invalid result path")
	}
	return path, nil
}

func normalizeResultID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "result_")
	id = strings.TrimSuffix(id, ".blob")
	return id
}

func validResultDigest(id string) bool {
	if len(id) != sha256.Size*2 || strings.ToLower(id) != id {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == sha256.Size
}

func readVerifiedResultBlob(path, expectedDigest string) ([]byte, os.FileInfo, error) {
	lexical, err := os.Lstat(path)
	if err != nil || lexical.Mode()&os.ModeSymlink != 0 || !lexical.Mode().IsRegular() || lexical.Size() <= 0 || lexical.Size() > maxSessionResultBlobBytes {
		return nil, nil, fmt.Errorf("invalid session result blob")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(lexical, opened) {
		return nil, nil, fmt.Errorf("session result blob changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSessionResultBlobBytes+1))
	if err != nil || int64(len(data)) != lexical.Size() || len(data) > maxSessionResultBlobBytes {
		return nil, nil, fmt.Errorf("session result blob changed while reading")
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != expectedDigest {
		return nil, nil, fmt.Errorf("session result blob digest mismatch")
	}
	return data, opened, nil
}

func verifyResultBlob(path, expectedDigest string) error {
	_, _, err := readVerifiedResultBlob(path, expectedDigest)
	return err
}

// LoadResult reads and verifies a stored result by content digest.
func (sm *SessionMemory) LoadResult(id string) (string, error) {
	if sm == nil || sm.initErr != nil || sm.mu == nil {
		return "", fmt.Errorf("session result store unavailable")
	}
	digest := normalizeResultID(id)
	path, err := sm.blobPath(digest)
	if err != nil {
		return "", err
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	data, _, err := readVerifiedResultBlob(path, digest)
	if err != nil {
		return "", fmt.Errorf("result not found or invalid")
	}
	return string(data), nil
}

// ReadResult returns one bounded byte range from the fully redacted view of a
// verified result. Redacting before pagination preserves PEM/token/assignment
// context even when the requested offset starts in the middle of a secret.
func (sm *SessionMemory) ReadResult(id string, offset int64, limit int) (ToolResultChunk, error) {
	if sm == nil || sm.initErr != nil || sm.mu == nil {
		return ToolResultChunk{}, fmt.Errorf("session result store unavailable")
	}
	if offset < 0 || limit < 0 || limit > maxToolResultReadBytes {
		return ToolResultChunk{}, fmt.Errorf("invalid result read range")
	}
	if limit == 0 {
		limit = defaultToolResultReadBytes
	}
	digest := normalizeResultID(id)
	path, err := sm.blobPath(digest)
	if err != nil {
		return ToolResultChunk{}, fmt.Errorf("invalid result id")
	}

	sm.mu.RLock()
	data, _, err := readVerifiedResultBlob(path, digest)
	sm.mu.RUnlock()
	if err != nil {
		return ToolResultChunk{}, fmt.Errorf("result not found or invalid")
	}
	redacted := redactSensitiveString(string(data))
	view := []byte(redacted)
	total := int64(len(view))
	if offset > total {
		return ToolResultChunk{}, fmt.Errorf("invalid result read range")
	}
	if offset < total && offset > 0 && view[offset]&0xc0 == 0x80 {
		return ToolResultChunk{}, fmt.Errorf("invalid result read range")
	}
	end := offset + int64(limit)
	if end > total {
		end = total
	}
	for end > offset && !utf8.Valid(view[offset:end]) {
		end--
	}
	content := string(view[offset:end])
	return ToolResultChunk{
		ID:         "result_" + digest,
		Content:    content,
		Offset:     offset,
		NextOffset: end,
		TotalBytes: total,
		EOF:        end == total,
	}, nil
}

var (
	exactStoredToolResultURIPattern = regexp.MustCompile(`^tool-result://sha256:([a-f0-9]{64})$`)
	storedToolResultHeaderPattern   = regexp.MustCompile(`^\[Tool result stored: result_([a-f0-9]{64}) \([0-9]+ bytes, tool=[^\r\n]*\); reference=tool-result://sha256:([a-f0-9]{64})\]`)
)

func canonicalStoredToolResultDigest(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if match := exactStoredToolResultURIPattern.FindStringSubmatch(value); len(match) == 2 && validResultDigest(match[1]) {
		return match[1], true
	}
	match := storedToolResultHeaderPattern.FindStringSubmatch(value)
	if len(match) != 3 || match[1] != match[2] || !validResultDigest(match[1]) {
		return "", false
	}
	return match[1], true
}

func committedToolResultDigests(messages []SessionMessage) []string {
	seen := make(map[string]struct{})
	for _, message := range messages {
		// Only committed tool observations are authoritative. User or
		// assistant text can mention a known orphan digest and must not promote
		// that CAS-losing blob into inventory or a fork.
		if message.Role != "tool" {
			continue
		}
		for _, value := range []string{message.Content, message.ToolResult} {
			if digest, ok := canonicalStoredToolResultDigest(value); ok {
				seen[digest] = struct{}{}
			}
		}
	}
	digests := make([]string, 0, len(seen))
	for digest := range seen {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	return digests
}

// ReferencesForMessages intersects committed transcript references with
// verified blobs in this exact store. Speculative or CAS-losing blobs are not
// returned merely because they exist on disk.
func (sm *SessionMemory) ReferencesForMessages(messages []SessionMessage) []ToolResultReference {
	if sm == nil || sm.initErr != nil || sm.mu == nil {
		return nil
	}
	digests := committedToolResultDigests(messages)
	if len(digests) == 0 {
		return nil
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	results := make([]ToolResultReference, 0, len(digests))
	for _, digest := range digests {
		path, err := sm.blobPath(digest)
		if err != nil {
			continue
		}
		data, _, err := readVerifiedResultBlob(path, digest)
		if err != nil {
			continue
		}
		results = append(results, ToolResultReference{
			ID:     "result_" + digest,
			Digest: "sha256:" + digest,
			Size:   int64(len(data)),
		})
	}
	return results
}

// CloneResultsTo copies only the requested verified content-addressed blobs
// into another exact session namespace. A hard link is preferred; verified
// atomic publication is used as a portable fallback.
func (sm *SessionMemory) CloneResultsTo(target *SessionMemory, digests []string) error {
	if sm == nil || target == nil || sm.initErr != nil || target.initErr != nil ||
		sm.mu == nil || target.mu == nil || sm.dir == "" || target.dir == "" {
		return fmt.Errorf("session result store unavailable")
	}
	unlock := lockSessionMemoryPair(sm, target)
	defer unlock()
	if err := validateSessionMemoryDirectory(sm.root, sm.dir); err != nil {
		return err
	}
	if err := validateSessionMemoryDirectory(target.root, target.dir); err != nil {
		return err
	}
	for _, rawID := range digests {
		digest := normalizeResultID(rawID)
		if !validResultDigest(digest) {
			return fmt.Errorf("invalid result id")
		}
		sourcePath, err := sm.blobPath(digest)
		if err != nil {
			return fmt.Errorf("invalid result id")
		}
		data, _, err := readVerifiedResultBlob(sourcePath, digest)
		if err != nil {
			return fmt.Errorf("committed result not found or invalid")
		}
		targetPath, err := target.blobPath(digest)
		if err != nil {
			return fmt.Errorf("invalid result id")
		}
		if _, err := os.Lstat(targetPath); err == nil {
			if verifyResultBlob(targetPath, digest) != nil {
				return fmt.Errorf("target result is invalid")
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect target result")
		}
		count, total, err := target.usageLocked()
		if err != nil {
			return err
		}
		if count >= maxSessionResultBlobs || total+int64(len(data)) > maxSessionResultStoreSize {
			return fmt.Errorf("session result store quota exceeded")
		}
		if err := os.Link(sourcePath, targetPath); err == nil {
			if err := syncSessionMemoryDirectory(target.dir); err != nil {
				return err
			}
			if err := verifyResultBlob(targetPath, digest); err != nil {
				return fmt.Errorf("cloned result is invalid")
			}
			continue
		}
		if err := target.storeBlobLocked(digest, string(data)); err != nil {
			return fmt.Errorf("clone committed result")
		}
	}
	return nil
}

func lockSessionMemoryPair(left, right *SessionMemory) func() {
	if left.mu == right.mu {
		left.mu.Lock()
		return left.mu.Unlock
	}
	first, second := left, right
	if canonicalCasePath(first.dir) > canonicalCasePath(second.dir) {
		first, second = second, first
	}
	first.mu.Lock()
	second.mu.Lock()
	return func() {
		second.mu.Unlock()
		first.mu.Unlock()
	}
}

// ListResults returns deterministic, verified blob metadata. Unknown files,
// symlinks, and corrupt blobs are omitted.
func (sm *SessionMemory) ListResults() []map[string]interface{} {
	if sm == nil || sm.initErr != nil || sm.mu == nil {
		return nil
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	entries, err := os.ReadDir(sm.dir)
	if err != nil {
		return nil
	}
	results := make([]map[string]interface{}, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "result_") || !strings.HasSuffix(name, ".blob") {
			continue
		}
		digest := normalizeResultID(name)
		path, err := sm.blobPath(digest)
		data, _, readErr := readVerifiedResultBlob(path, digest)
		if err != nil || readErr != nil {
			continue
		}
		results = append(results, map[string]interface{}{
			"id":   "result_" + digest,
			"size": int64(len(data)),
		})
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i]["id"].(string) < results[j]["id"].(string)
	})
	return results
}

func (sm *SessionMemory) usageLocked() (int, int64, error) {
	entries, err := os.ReadDir(sm.dir)
	if err != nil {
		return 0, 0, err
	}
	count := 0
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "result_") || !strings.HasSuffix(name, ".blob") {
			continue
		}
		digest := normalizeResultID(name)
		path, pathErr := sm.blobPath(digest)
		data, _, readErr := readVerifiedResultBlob(path, digest)
		if pathErr != nil || readErr != nil {
			return 0, 0, fmt.Errorf("session result store contains an invalid blob")
		}
		count++
		total += int64(len(data))
	}
	return count, total, nil
}

// Cleanup preserves the compatibility signature and intentionally ignores the
// error. New callers that need evidence should use CleanupSafe.
func (sm *SessionMemory) Cleanup() { _ = sm.CleanupSafe() }

// CleanupSafe removes only recognized temporary and verified blob files from
// the exact digest directory; it never recursively deletes a computed path.
func (sm *SessionMemory) CleanupSafe() error {
	if sm == nil || sm.initErr != nil || sm.mu == nil || sm.dir == "" || sm.root == "" {
		return fmt.Errorf("session result store unavailable")
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if err := validateSessionMemoryDirectory(sm.root, sm.dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(sm.dir)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(sm.dir, name)
		if strings.HasPrefix(name, ".tmp_") {
			info, infoErr := entry.Info()
			if infoErr != nil || entry.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("unsafe session result temp entry")
			}
			paths = append(paths, path)
			continue
		}
		digest := normalizeResultID(name)
		if !strings.HasPrefix(name, "result_") || !strings.HasSuffix(name, ".blob") || !validResultDigest(digest) || verifyResultBlob(path, digest) != nil {
			return fmt.Errorf("unrecognized session result entry")
		}
		paths = append(paths, path)
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if err := os.Remove(sm.dir); err != nil {
		return err
	}
	return syncSessionMemoryDirectory(sm.root)
}

// TotalSize returns the verified bytes in this session's store.
func (sm *SessionMemory) TotalSize() int64 {
	if sm == nil || sm.initErr != nil || sm.mu == nil {
		return 0
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	_, total, err := sm.usageLocked()
	if err != nil {
		return 0
	}
	return total
}

type SessionMemoryStats struct {
	ResultCount int    `json:"resultCount"`
	TotalBytes  int64  `json:"totalBytes"`
	SessionDir  string `json:"sessionDir"`
}

func (sm *SessionMemory) Stats() SessionMemoryStats {
	if sm == nil || sm.initErr != nil || sm.mu == nil {
		return SessionMemoryStats{}
	}
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	count, total, err := sm.usageLocked()
	if err != nil {
		return SessionMemoryStats{}
	}
	return SessionMemoryStats{
		ResultCount: count,
		TotalBytes:  total,
		SessionDir:  filepath.Base(sm.dir),
	}
}

func createCanonicalDirectory(path string, mode os.FileMode) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if err := rejectExistingSessionMemorySymlinks(absolute); err != nil {
		return "", err
	}

	// Find the nearest existing ancestor before creating anything. This avoids
	// os.MkdirAll following an intermediate symlink and producing state outside
	// the requested lexical path. The existing ancestor is canonicalized first,
	// which also normalizes ordinary Windows 8.3 path components.
	current := absolute
	missing := make([]string, 0, 4)
	for {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return "", fmt.Errorf("directory path contains a non-directory or symlink")
			}
			break
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("directory has no existing ancestor")
		}
		component := filepath.Base(current)
		if component == "." || component == ".." || filepath.Base(component) != component {
			return "", fmt.Errorf("invalid directory component")
		}
		missing = append(missing, component)
		current = parent
	}

	canonical, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for index := len(missing) - 1; index >= 0; index-- {
		candidate := filepath.Join(canonical, missing[index])
		if err := os.Mkdir(candidate, mode); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, statErr := os.Lstat(candidate)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("created directory is not a regular directory")
		}
		resolved, resolveErr := filepath.EvalSymlinks(candidate)
		if resolveErr != nil || !sameSessionMemoryPath(candidate, resolved) {
			return "", fmt.Errorf("created directory changed identity")
		}
		canonical = filepath.Clean(resolved)
	}
	if err := os.Chmod(canonical, mode); err != nil {
		return "", err
	}
	return canonical, nil
}

// rejectExistingSessionMemorySymlinks checks every lexical ancestor before
// creation. Checking only the final existing path is insufficient when a
// regular descendant is reached through an intermediate symlink.
func rejectExistingSessionMemorySymlinks(path string) error {
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				resolved, resolveErr := filepath.EvalSymlinks(current)
				if resolveErr != nil || !allowedSessionMemoryRootAlias(runtime.GOOS, current, resolved) {
					return fmt.Errorf("directory path contains a symlink")
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

// macOS exposes ordinary temporary paths through root-owned /var and /tmp
// aliases into /private. Those two fixed system aliases are not caller-owned
// path redirections. Every nested or user-controlled symlink remains rejected.
func allowedSessionMemoryRootAlias(goos, lexical, resolved string) bool {
	if goos != "darwin" {
		return false
	}
	lexical = pathpkg.Clean(filepath.ToSlash(lexical))
	resolved = pathpkg.Clean(filepath.ToSlash(resolved))
	switch lexical {
	case "/var":
		return resolved == "/private/var"
	case "/tmp":
		return resolved == "/private/tmp"
	default:
		return false
	}
}

func createDirectChildDirectory(parent, child string, mode os.FileMode) (string, error) {
	if filepath.Base(child) != child || child == "." || child == ".." {
		return "", fmt.Errorf("invalid child directory")
	}
	path := filepath.Join(parent, child)
	if err := os.Mkdir(path, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("child is not a regular directory")
	}
	if err := os.Chmod(path, mode); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !sameSessionMemoryPath(path, resolved) {
		return "", fmt.Errorf("child directory resolves outside its lexical path")
	}
	relative, err := filepath.Rel(parent, resolved)
	if err != nil || relative != child {
		return "", fmt.Errorf("child directory escaped parent")
	}
	return filepath.Clean(resolved), nil
}

func validateSessionMemoryDirectory(root, dir string) error {
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil || !sameSessionMemoryPath(root, rootResolved) {
		return fmt.Errorf("invalid session result root")
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil || dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return fmt.Errorf("invalid session result directory")
	}
	dirResolved, err := filepath.EvalSymlinks(dir)
	if err != nil || !sameSessionMemoryPath(dir, dirResolved) {
		return fmt.Errorf("invalid session result directory")
	}
	relative, err := filepath.Rel(rootResolved, dirResolved)
	if err != nil || filepath.Dir(relative) != "." || !strings.HasPrefix(filepath.Base(relative), "sm_") {
		return fmt.Errorf("session result directory escaped root")
	}
	return nil
}

func sameSessionMemoryPath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func canonicalCasePath(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func syncSessionMemoryDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func boundedUTF8Prefix(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func sanitizeResultToolName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var builder strings.Builder
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			continue
		}
		builder.WriteRune(character)
		if builder.Len() >= 128 {
			break
		}
	}
	if builder.Len() == 0 {
		return "unknown"
	}
	return builder.String()
}

// ExecuteToolWithMemory is the compatibility wrapper around the explicit
// options path.
func ExecuteToolWithMemory(name string, input json.RawMessage, workDir string, mem *SessionMemory) (string, bool) {
	runner, policy := configuredSandboxExecution(nil, sandbox.Policy{}, "session memory")
	return ExecuteToolWithMemoryOptions(name, input, workDir, mem, ToolExecutionOptions{
		Context:       context.Background(),
		SandboxRunner: runner,
		SandboxPolicy: policy,
	})
}

func ExecuteToolWithMemoryOptions(
	name string,
	input json.RawMessage,
	workDir string,
	mem *SessionMemory,
	opts ToolExecutionOptions,
) (string, bool) {
	opts.SandboxRunner, opts.SandboxPolicy = configuredSandboxExecution(
		opts.SandboxRunner,
		opts.SandboxPolicy,
		"session memory",
	)
	result, isError := ExecuteToolWithOptions(name, input, workDir, opts)

	if mem != nil && !isError {
		reference, disposition, err := mem.StoreResultChecked(name, result)
		if err != nil {
			return toolResultPersistenceFailureMessage, true
		}
		if disposition == ToolResultPersisted {
			return reference, false
		}
	}
	return result, isError
}
