// Package updater contains the deliberately small, local artifact update
// transaction used by the explicit `corelaycode update` command.
//
// It does not discover releases, download artifacts, or run in the background.
// Callers must provide the artifact and its expected SHA-256 digest. The
// transaction keeps the previous executable as a rollback point until the
// caller decides to remove it.
package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrInvalidDigest = errors.New("invalid sha256 digest")
	ErrTargetInUse   = errors.New("target executable is in use")
	ErrRollback      = errors.New("update rollback failed")
)

type Result struct {
	ArtifactDigest string
	Target         string
	Backup         string
}

// VerifyArtifact verifies a regular, non-symlink artifact against the exact
// hexadecimal SHA-256 digest supplied by the release channel.
func VerifyArtifact(path, expected string) (string, error) {
	expected, err := normalizeDigest(expected)
	if err != nil {
		return "", err
	}
	file, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("read artifact: %w", err)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if digest != expected {
		return digest, fmt.Errorf("artifact digest mismatch: got %s", digest)
	}
	return digest, nil
}

// Install verifies source, stages it beside target, and atomically replaces
// target while retaining the old target at backup. If the second rename or
// post-install verification fails, the old target is restored.
func Install(source, target, expected, backup string) (Result, error) {
	if strings.TrimSpace(backup) == "" {
		backup = strings.TrimSpace(target) + ".previous"
	}
	source, target, backup, err := normalizePaths(source, target, backup)
	if err != nil {
		return Result{}, err
	}
	digest, err := VerifyArtifact(source, expected)
	if err != nil {
		return Result{}, err
	}
	if err := rejectSymlink(target); err != nil {
		return Result{}, err
	}
	if err := rejectSymlink(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	if _, err := os.Stat(backup); err == nil {
		return Result{}, fmt.Errorf("backup already exists: %s", backup)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("inspect backup: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return Result{}, fmt.Errorf("create target directory: %w", err)
	}
	staged, err := stage(source, target)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.Remove(staged) }()

	if err := os.Rename(target, backup); err != nil {
		if isTargetInUse(err) {
			return Result{}, fmt.Errorf("%w: %v", ErrTargetInUse, err)
		}
		return Result{}, fmt.Errorf("move current executable to backup: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		if rollbackErr := os.Rename(backup, target); rollbackErr != nil {
			return Result{}, fmt.Errorf("%w: install=%v restore=%v", ErrRollback, err, rollbackErr)
		}
		if isTargetInUse(err) {
			return Result{}, fmt.Errorf("%w: %v", ErrTargetInUse, err)
		}
		return Result{}, fmt.Errorf("install staged executable: %w", err)
	}
	installedDigest, verifyErr := VerifyArtifact(target, digest)
	if verifyErr != nil || installedDigest != digest {
		_ = os.Remove(target)
		if rollbackErr := os.Rename(backup, target); rollbackErr != nil {
			return Result{}, fmt.Errorf("%w: verify=%v restore=%v", ErrRollback, verifyErr, rollbackErr)
		}
		if verifyErr == nil {
			verifyErr = fmt.Errorf("installed digest mismatch")
		}
		return Result{}, fmt.Errorf("verify installed executable: %w", verifyErr)
	}
	return Result{ArtifactDigest: digest, Target: target, Backup: backup}, nil
}

// Rollback restores backup to target and keeps a failed/current target at a
// temporary sibling until the restore succeeds. It is intentionally explicit
// and never runs as a background action.
func Rollback(target, backup string) error {
	backup, target, _, err := normalizePaths(backup, target, "")
	if err != nil {
		return err
	}
	if err := rejectSymlink(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := rejectSymlink(backup); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".corelay-rollback-")
	if err != nil {
		return fmt.Errorf("prepare rollback: %w", err)
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("prepare rollback: %w", err)
	}
	_ = os.Remove(tempPath)
	if err := os.Rename(target, tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		if isTargetInUse(err) {
			return fmt.Errorf("%w: %v", ErrTargetInUse, err)
		}
		return fmt.Errorf("move current executable for rollback: %w", err)
	}
	if err := os.Rename(backup, target); err != nil {
		if _, statErr := os.Stat(tempPath); statErr == nil {
			_ = os.Rename(tempPath, target)
		}
		if isTargetInUse(err) {
			return fmt.Errorf("%w: %v", ErrTargetInUse, err)
		}
		return fmt.Errorf("restore backup: %w", err)
	}
	_ = os.Remove(tempPath)
	return nil
}

func normalizePaths(source, target, backup string) (string, string, string, error) {
	values := []*string{&source, &target}
	if strings.TrimSpace(backup) != "" {
		values = append(values, &backup)
	}
	for _, value := range values {
		if strings.TrimSpace(*value) == "" {
			return "", "", "", fmt.Errorf("path is required")
		}
		absolute, err := filepath.Abs(*value)
		if err != nil {
			return "", "", "", fmt.Errorf("normalize path: %w", err)
		}
		*value = filepath.Clean(absolute)
	}
	if samePath(source, target) || (backup != "" && (samePath(source, backup) || samePath(target, backup))) {
		return "", "", "", fmt.Errorf("artifact, target, and backup must be different files")
	}
	return source, target, backup, nil
}

func stage(source, target string) (string, error) {
	input, err := openRegular(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	mode := os.FileMode(0o755)
	if info, statErr := input.Stat(); statErr == nil && info.Mode().Perm() != 0 {
		mode = info.Mode().Perm()
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".corelay-update-*")
	if err != nil {
		return "", fmt.Errorf("stage artifact: %w", err)
	}
	staged := file.Name()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		_ = os.Remove(staged)
		return "", fmt.Errorf("set staged artifact mode: %w", err)
	}
	removeOnError := true
	defer func() {
		_ = file.Close()
		if removeOnError {
			_ = os.Remove(staged)
		}
	}()
	if _, err := io.Copy(file, input); err != nil {
		return "", fmt.Errorf("stage artifact: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("flush staged artifact: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close staged artifact: %w", err)
	}
	removeOnError = false
	return staged, nil
}

func openRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect artifact %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact must be a regular non-symlink file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open artifact %q: %w", path, err)
	}
	return file, nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path must not be a symlink: %s", path)
	}
	return nil
}

func normalizeDigest(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != sha256.Size*2 {
		return "", ErrInvalidDigest
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", ErrInvalidDigest
	}
	return value, nil
}

func samePath(left, right string) bool {
	if right == "" {
		return false
	}
	if filepath.VolumeName(left) != "" || filepath.VolumeName(right) != "" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func isTargetInUse(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "used by another process") || strings.Contains(message, "being used") || strings.Contains(message, "permission denied")
}
