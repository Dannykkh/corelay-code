package updater

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallVerifiesStagesAndKeepsRollbackCopy(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, "corelaycode-new")
	target := filepath.Join(root, "corelaycode")
	backup := filepath.Join(root, "corelaycode.previous")
	writeUpdaterFile(t, artifact, "new executable")
	writeUpdaterFile(t, target, "old executable")
	digest := updaterDigest(t, artifact)

	result, err := Install(artifact, target, digest, backup)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if result.ArtifactDigest != digest || result.Target != target || result.Backup != backup {
		t.Fatalf("result = %+v", result)
	}
	assertUpdaterFile(t, target, "new executable")
	assertUpdaterFile(t, backup, "old executable")
	if err := Rollback(target, backup); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	assertUpdaterFile(t, target, "old executable")
}

func TestInstallRejectsTamperedArtifactAndExistingBackup(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, "new")
	target := filepath.Join(root, "current")
	writeUpdaterFile(t, artifact, "artifact")
	writeUpdaterFile(t, target, "current")
	if _, err := Install(artifact, target, strings.Repeat("0", 64), ""); err == nil {
		t.Fatal("tampered artifact was accepted")
	}
	backup := target + ".previous"
	writeUpdaterFile(t, backup, "reserved")
	if _, err := Install(artifact, target, updaterDigest(t, artifact), backup); err == nil {
		t.Fatal("existing backup was overwritten")
	}
}

func TestVerifyArtifactRejectsInvalidDigestAndSymlink(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, "artifact")
	writeUpdaterFile(t, artifact, "payload")
	if _, err := VerifyArtifact(artifact, "bad"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("invalid digest error = %v", err)
	}
	symlink := filepath.Join(root, "link")
	if err := os.Symlink(artifact, symlink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := VerifyArtifact(symlink, updaterDigest(t, artifact)); err == nil {
		t.Fatal("symlink artifact was accepted")
	}
}

func writeUpdaterFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func updaterDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:])
}

func assertUpdaterFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}
