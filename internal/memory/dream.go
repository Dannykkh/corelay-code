package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Background consolidation ("autoDream") — mirror of Claude Code's
// services/autoDream package, scaled down to what Corelay Code needs.
//
// The three gates run cheapest-first so a hot path never does a full
// scan unless it already believes consolidation is due:
//
//  1. Time gate   — at least MinSinceLast has elapsed since last run.
//  2. Count gate  — at least MinNewEntries new auto-entries have piled up.
//  3. Lock gate   — no other process is currently consolidating.
//
// Like extraction, this file does NOT call an LLM. It produces the
// prompt, parses the response, and persists the result. The actual
// model call happens in the agent layer so memory stays dependency-free.

// DreamConfig controls the consolidation gate. Values chosen to match
// Claude Code's autoDream defaults: MinSinceLast = 24h, MinNewEntries = 5,
// StaleLockAge = 10 minutes (the age at which an abandoned lock is evicted).
type DreamConfig struct {
	MinSinceLast  time.Duration
	MinNewEntries int
	StaleLockAge  time.Duration
}

// DefaultDreamConfig returns the recommended production settings.
func DefaultDreamConfig() DreamConfig {
	return DreamConfig{
		MinSinceLast:  24 * time.Hour,
		MinNewEntries: 5,
		StaleLockAge:  10 * time.Minute,
	}
}

// GateReason enumerates why a gate check passed or did not. Exposed as
// strings so logs and UI can present them without a translation layer.
const (
	ReasonReady       = "ok"
	ReasonTooSoon     = "too-soon"
	ReasonTooFewNew   = "too-few-new"
	ReasonLocked      = "locked"
	ReasonScanFailed  = "scan-failed"
	ReasonStateFailed = "state-failed"
)

// GateResult reports whether autoDream should run now. NewEntries is
// populated whenever the scan succeeds so callers can surface "5 new
// memories since last consolidation" even when the time gate blocks.
type GateResult struct {
	Ready      bool
	Reason     string
	NewEntries int
	Elapsed    time.Duration
}

// CheckDreamGate runs the three gates for workDir against cfg. Returns
// an error only for I/O failures that prevent a meaningful decision —
// a "not yet" answer is reported via GateResult.Reason, not error.
func CheckDreamGate(workDir string, cfg DreamConfig) (GateResult, error) {
	state, err := readDreamState(workDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return GateResult{Reason: ReasonStateFailed}, fmt.Errorf("memory: read dream state: %w", err)
	}

	// Time gate — skip if we ran too recently. A zero LastConsolidated
	// means "never", which always passes this gate.
	var elapsed time.Duration
	if state.LastConsolidatedUnix > 0 {
		elapsed = time.Since(time.Unix(state.LastConsolidatedUnix, 0))
		if elapsed < cfg.MinSinceLast {
			return GateResult{Reason: ReasonTooSoon, Elapsed: elapsed}, nil
		}
	}

	// Count gate — require a minimum number of NEW entries since last
	// consolidation. We compute this from a live scan so we do not trust
	// stale metadata.
	entries, err := Scan(workDir)
	if err != nil {
		return GateResult{Reason: ReasonScanFailed}, fmt.Errorf("memory: scan: %w", err)
	}
	newCount := len(entries) - state.LastEntryCount
	if newCount < 0 {
		// Rare: user manually deleted files. Treat as "everything is new".
		newCount = len(entries)
	}
	if newCount < cfg.MinNewEntries {
		return GateResult{
			Reason:     ReasonTooFewNew,
			NewEntries: newCount,
			Elapsed:    elapsed,
		}, nil
	}

	// Lock gate — do not start if another process is consolidating.
	// Stale locks (older than StaleLockAge) are ignored but NOT removed
	// here — removal happens lazily during lock acquisition.
	locked, err := isDreamLocked(workDir, cfg.StaleLockAge)
	if err != nil {
		return GateResult{Reason: ReasonStateFailed}, err
	}
	if locked {
		return GateResult{
			Reason:     ReasonLocked,
			NewEntries: newCount,
			Elapsed:    elapsed,
		}, nil
	}

	return GateResult{
		Ready:      true,
		Reason:     ReasonReady,
		NewEntries: newCount,
		Elapsed:    elapsed,
	}, nil
}

// TryAcquireDreamLock creates an exclusive lock file so at most one
// process consolidates at a time. Returns a release function that
// removes the lock; callers MUST defer release even on error paths that
// take different branches later. An existing lock older than
// cfg.StaleLockAge is evicted before the create is attempted.
//
// This is NOT a cross-host lock — it is cooperative within a single
// filesystem. That is the same guarantee Claude Code's
// consolidationLock.ts provides, and it is sufficient for Corelay Code's
// single-user-per-machine model.
func TryAcquireDreamLock(workDir string, cfg DreamConfig) (release func(), err error) {
	if err := Ensure(workDir); err != nil {
		return nil, err
	}
	lockPath := dreamLockPath(workDir)

	if cfg.StaleLockAge > 0 {
		if info, err := os.Stat(lockPath); err == nil {
			if time.Since(info.ModTime()) > cfg.StaleLockAge {
				_ = os.Remove(lockPath)
			}
		}
	}

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("memory: acquire dream lock: %w", err)
	}
	// The body of the lock is advisory — who is holding it and since
	// when — so a human debugging an abandoned lock has context.
	_, _ = fmt.Fprintf(f, "pid=%d\ntime=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	_ = f.Close()

	return func() { _ = os.Remove(lockPath) }, nil
}

// MarkConsolidated rewrites the dream state file with the current
// time and the current entry count. Call this after a successful
// ApplyConsolidation so the next CheckDreamGate reflects the new baseline.
func MarkConsolidated(workDir string) error {
	entries, err := Scan(workDir)
	if err != nil {
		return err
	}
	return writeDreamState(workDir, dreamState{
		LastConsolidatedUnix: time.Now().Unix(),
		LastEntryCount:       len(entries),
	})
}

// BuildConsolidatePrompt returns the prompt for the consolidation model
// call. The current set of auto-generated entries is serialized inline
// so the model has everything it needs without follow-up tool calls.
func BuildConsolidatePrompt(workDir string) (string, error) {
	entries, err := Scan(workDir)
	if err != nil {
		return "", err
	}

	var rendered strings.Builder
	if len(entries) == 0 {
		rendered.WriteString("(no entries yet)")
	} else {
		for _, e := range entries {
			rendered.WriteString("### ")
			rendered.WriteString(e.Name)
			rendered.WriteString(" (")
			rendered.WriteString(string(e.Type))
			rendered.WriteString(")\n\n")
			rendered.WriteString(e.Description)
			rendered.WriteString("\n\n")
			rendered.WriteString(strings.TrimSpace(e.Body))
			rendered.WriteString("\n\n---\n\n")
		}
	}

	var b strings.Builder
	b.WriteString(consolidateSystemInstructions)
	b.WriteString("\n\n## Current entries\n\n")
	b.WriteString(rendered.String())
	b.WriteString("\n## Your response\n\n")
	b.WriteString("Return ONLY a JSON array as specified.\n")
	return b.String(), nil
}

// consolidateSystemInstructions is the static prefix of the consolidation
// prompt. Distinct from extraction: the goal here is pruning, merging,
// and clarifying an existing set rather than discovering new facts.
const consolidateSystemInstructions = `You are a memory consolidation assistant. Review the current set of
durable memories and return a cleaned-up version as a JSON array.

Goals:
- Merge near-duplicates into a single clearer entry.
- Drop entries that have become redundant (already covered by another,
  or obviously stale — e.g. a deadline that has passed).
- Clarify entry bodies so future reads are faster.
- Preserve the same four types: user, feedback, project, reference.

Rules:
- Return ONLY a JSON array. No code fences, no prose.
- Fewer but clearer entries is better than the same count or more.
- Do NOT invent facts that were not in the input.
- Do NOT change the "type" field unless the input was obviously
  mis-classified.
- "name" should be stable — reuse existing names when in doubt so the
  on-disk slug stays the same as before.

Format (same as extraction):
    [{"name":"...","description":"...","type":"...","body":"..."}]`

// ApplyConsolidation replaces all auto-generated entries with the
// consolidated set and refreshes MEMORY.md. Scaffold files (anything
// that does not start with one of the four type prefixes) are NEVER
// touched — those belong to the user and must survive consolidation.
//
// The sequence is:
//  1. Remove existing auto entry files in the memory dir.
//  2. Persist the consolidated entries (dedupe via SaveExtracted).
//  3. MarkConsolidated so the next gate sees the new baseline.
func ApplyConsolidation(workDir string, consolidated []Entry) error {
	dir := MemoryDir(workDir)
	existing, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("memory: read memory dir: %w", err)
	}
	for _, de := range existing {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".md") {
			continue
		}
		if !IsAutoEntryFilename(de.Name()) {
			continue // scaffold — leave alone
		}
		if err := os.Remove(filepath.Join(dir, de.Name())); err != nil {
			return fmt.Errorf("memory: remove %s: %w", de.Name(), err)
		}
	}

	if _, _, err := SaveExtracted(workDir, consolidated); err != nil {
		return err
	}
	return MarkConsolidated(workDir)
}

// IsAutoEntryFilename reports whether a filename is one of the four
// auto-extractor outputs (type prefix + sanitized slug + .md). Exposed
// so tests and the agent layer can reason about which files are
// tool-managed vs hand-edited without re-implementing the rule.
func IsAutoEntryFilename(name string) bool {
	for _, t := range []Type{TypeUser, TypeFeedback, TypeProject, TypeReference} {
		if strings.HasPrefix(name, string(t)+"_") && strings.HasSuffix(name, ".md") {
			return true
		}
	}
	return false
}

// ------------------- dream state persistence -------------------

// dreamState is the content of <memory>/.dream-state.json.
type dreamState struct {
	LastConsolidatedUnix int64 `json:"lastConsolidatedUnix"`
	LastEntryCount       int   `json:"lastEntryCount"`
}

func dreamStatePath(workDir string) string {
	return filepath.Join(MemoryDir(workDir), ".dream-state.json")
}

func dreamLockPath(workDir string) string {
	return filepath.Join(MemoryDir(workDir), ".dream-lock")
}

func readDreamState(workDir string) (dreamState, error) {
	data, err := os.ReadFile(dreamStatePath(workDir))
	if err != nil {
		return dreamState{}, err
	}
	var s dreamState
	if err := json.Unmarshal(data, &s); err != nil {
		return dreamState{}, fmt.Errorf("memory: parse dream state: %w", err)
	}
	return s, nil
}

func writeDreamState(workDir string, s dreamState) error {
	if err := Ensure(workDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// Atomic via tmp+rename like the other writers in this package.
	path := dreamStatePath(workDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func isDreamLocked(workDir string, staleLockAge time.Duration) (bool, error) {
	info, err := os.Stat(dreamLockPath(workDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if staleLockAge > 0 && time.Since(info.ModTime()) > staleLockAge {
		return false, nil // effectively unlocked
	}
	return true, nil
}
