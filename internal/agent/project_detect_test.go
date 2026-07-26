package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestDetectProjectFallsBackToExtensionsWithoutMarkerFile(t *testing.T) {
	// A plain folder of Python sources: no pyproject.toml, no requirements.txt.
	// This used to come back "unknown", which made the Test tool answer
	// "no test runner configured" and report it as a success.
	dir := t.TempDir()
	writeFiles(t, dir, "config.py", "report.py", "test_report.py")

	if got := DetectProject(dir).Type; got != "python" {
		t.Fatalf("Type = %q, want %q", got, "python")
	}
}

func TestDetectProjectPrefersMarkerFileOverExtensions(t *testing.T) {
	// go.mod wins even when Python sources outnumber Go ones.
	dir := t.TempDir()
	writeFiles(t, dir, "go.mod", "a.py", "b.py", "c.py")

	if got := DetectProject(dir).Type; got != "go" {
		t.Fatalf("Type = %q, want %q", got, "go")
	}
}

func TestDetectProjectStaysUnknownWithoutSources(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "notes.txt", "image.png")

	if got := DetectProject(dir).Type; got != "unknown" {
		t.Fatalf("Type = %q, want %q", got, "unknown")
	}
}

func TestPythonExecutableResolvesToSomethingOnPath(t *testing.T) {
	// Whichever name exists on this machine, the result must not be empty —
	// an empty command name turns into an opaque exec failure.
	if got := pythonExecutable(); got != "python3" && got != "python" {
		t.Fatalf("pythonExecutable() = %q, want python3 or python", got)
	}
}
