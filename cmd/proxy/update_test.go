package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunUpdateRequiresExplicitVerifiedArtifact(t *testing.T) {
	if code := runUpdate(nil); code != 2 {
		t.Fatalf("missing arguments exit code = %d, want 2", code)
	}
	root := t.TempDir()
	artifact := filepath.Join(root, "artifact")
	if err := os.WriteFile(artifact, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := runUpdate([]string{"-artifact", artifact, "-sha256", strings.Repeat("0", 64), "-dry-run"}); code != 1 {
		t.Fatalf("tampered artifact exit code = %d, want 1", code)
	}
}

func TestRunUpdateInstallsLocalArtifactAndPrintsRollback(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, "artifact")
	target := filepath.Join(root, "current")
	backup := filepath.Join(root, "previous")
	if err := os.WriteFile(artifact, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("new"))
	// runUpdate intentionally writes to process streams to match the other
	// top-level diagnostics subcommands. The package-level transaction is the
	// deterministic seam; this test verifies the command's install result.
	code := runUpdate([]string{"-artifact", artifact, "-sha256", fmt.Sprintf("%x", digest[:]), "-target", target, "-backup", backup})
	if code != 0 {
		t.Fatalf("runUpdate exit code = %d", code)
	}
	updated, err := os.ReadFile(target)
	if err != nil || string(updated) != "new" {
		t.Fatalf("updated target = %q, err=%v", updated, err)
	}
	previous, err := os.ReadFile(backup)
	if err != nil || string(previous) != "old" {
		t.Fatalf("rollback copy = %q, err=%v", previous, err)
	}
	if code := runUpdate([]string{"-rollback", "-target", target, "-backup", backup}); code != 0 {
		t.Fatalf("rollback exit code = %d", code)
	}
	rolledBack, err := os.ReadFile(target)
	if err != nil || string(rolledBack) != "old" {
		t.Fatalf("rolled back target = %q, err=%v", rolledBack, err)
	}
}
