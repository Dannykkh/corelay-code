//go:build windows

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsSelfUpdateHelperReplacesAndRollsBackRunningExecutable(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "corelaycode.exe")
	artifact := filepath.Join(root, "corelaycode-next.exe")
	backup := filepath.Join(root, "corelaycode.previous")
	buildProxyFixture(t, target, "")
	oldDigest := fileSHA256(t, target)
	buildProxyFixture(t, artifact, "-X github.com/Dannykkh/corelay-code/internal/buildinfo.Version=windows-update-fixture")
	newDigest := fileSHA256(t, artifact)
	if oldDigest == newDigest {
		t.Fatal("fixture binaries unexpectedly have the same digest")
	}

	output, err := exec.Command(
		target,
		"update",
		"-artifact", artifact,
		"-sha256", newDigest,
		"-target", target,
		"-backup", backup,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("self-update command failed: %v\n%s", err, output)
	}
	waitForDigest(t, target, newDigest)
	if got := fileSHA256(t, backup); got != oldDigest {
		t.Fatalf("rollback digest = %s, want original %s", got, oldDigest)
	}

	output, err = exec.Command(
		target,
		"update",
		"-rollback",
		"-target", target,
		"-backup", backup,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("self-rollback command failed: %v\n%s", err, output)
	}
	waitForDigest(t, target, oldDigest)
}

func buildProxyFixture(t *testing.T, output, ldflags string) {
	t.Helper()
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"build", "-o", output}
	if strings.TrimSpace(ldflags) != "" {
		args = append(args, "-ldflags", ldflags)
	}
	args = append(args, ".")
	command := exec.Command("go", args...)
	command.Dir = workingDir
	// Use the toolchain selected by the parent test process. Hosted CI runs the
	// repository's declared Go version; the fixture must not force a separate
	// toolchain download just to build its two temporary binaries.
	command.Env = os.Environ()
	if buildOutput, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", filepath.Base(output), err, buildOutput)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("%x", digest[:])
}

func waitForDigest(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			digest := sha256.Sum256(data)
			if fmt.Sprintf("%x", digest[:]) == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not reach digest %s; got %s", path, want, fileSHA256(t, path))
}
