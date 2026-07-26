package agent

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/aniclew/aniclew/internal/config"
)

// Checkpoint / undo: before the agent's first edit in a turn the previous undo
// buffer is cleared (single-level, most-recent-turn undo); each edited file's
// prior state is backed up once. "/undo" restores them — files the agent
// created are deleted, files it modified are reverted. Backups live under
// <state dir>/undo/<hash> so the user's workspace stays clean.

func checkpointDir(workDir string) string {
	sum := sha1.Sum([]byte(workDir))
	return filepath.Join(config.BaseDir(), "undo", hex.EncodeToString(sum[:10]))
}

type ckptFile struct {
	Existed bool   `json:"existed"`
	Backup  string `json:"backup"` // backup filename within the dir ("" if file was created)
}

type ckptManifest struct {
	WorkDir string              `json:"workDir"`
	Files   map[string]ckptFile `json:"files"` // relPath -> info
}

func ckptManifestPath(workDir string) string {
	return filepath.Join(checkpointDir(workDir), "manifest.json")
}

func readManifest(workDir string) ckptManifest {
	m := ckptManifest{WorkDir: workDir, Files: map[string]ckptFile{}}
	if b, err := os.ReadFile(ckptManifestPath(workDir)); err == nil {
		json.Unmarshal(b, &m)
		if m.Files == nil {
			m.Files = map[string]ckptFile{}
		}
	}
	return m
}

func writeManifest(workDir string, m ckptManifest) {
	os.MkdirAll(checkpointDir(workDir), 0o755)
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		os.WriteFile(ckptManifestPath(workDir), b, 0o644)
	}
}

// startCheckpoint clears the previous undo buffer to begin a new generation.
func startCheckpoint(workDir string) {
	os.RemoveAll(checkpointDir(workDir))
	writeManifest(workDir, ckptManifest{WorkDir: workDir, Files: map[string]ckptFile{}})
}

// checkpointFile backs up a file's current (pre-edit) state once per generation.
func checkpointFile(workDir, relPath, absPath string) {
	m := readManifest(workDir)
	if _, done := m.Files[relPath]; done {
		return // already captured this generation
	}
	var entry ckptFile
	if b, err := os.ReadFile(absPath); err == nil {
		entry.Existed = true
		entry.Backup = hex.EncodeToString(sha1Sum(relPath)) + ".bak"
		os.WriteFile(filepath.Join(checkpointDir(workDir), entry.Backup), b, 0o644)
	}
	m.Files[relPath] = entry
	writeManifest(workDir, m)
}

func sha1Sum(s string) []byte {
	sum := sha1.Sum([]byte(s))
	return sum[:]
}

// undoCheckpoint reverts everything in the current undo buffer and clears it.
// Returns the reverted (relative) paths and whether anything was reverted.
func undoCheckpoint(workDir string) (reverted []string, ok bool) {
	m := readManifest(workDir)
	if len(m.Files) == 0 {
		return nil, false
	}
	for rel, f := range m.Files {
		abs := resolvePath(rel, workDir)
		if f.Existed {
			if b, err := os.ReadFile(filepath.Join(checkpointDir(workDir), f.Backup)); err == nil {
				if os.WriteFile(abs, b, 0o644) == nil {
					reverted = append(reverted, rel)
				}
			}
		} else if os.Remove(abs) == nil { // agent-created file — remove it
			reverted = append(reverted, rel+" (created → deleted)")
		}
	}
	os.RemoveAll(checkpointDir(workDir))
	return reverted, len(reverted) > 0
}
