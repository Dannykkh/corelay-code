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

func TestLooksLikePythonVersionRejectsWindowsStoreAlias(t *testing.T) {
	// Observed on Windows: python3.exe is an App Execution Alias that resolves
	// on PATH, exits 0, prints "Python", and runs nothing. Selecting it made the
	// Test tool return "Python\n[tests failed]" with no pytest output at all.
	cases := []struct {
		out  string
		want bool
	}{
		{"Python 3.12.4", true},
		{"Python 3.14.0rc1", true},
		{"python 3.9.7\n", true},
		{"Python", false},
		{"Python\n", false},
		{"", false},
		{"Microsoft Store", false},
	}

	for _, c := range cases {
		if got := looksLikePythonVersion([]byte(c.out)); got != c.want {
			t.Errorf("looksLikePythonVersion(%q) = %v, want %v", c.out, got, c.want)
		}
	}
}

func TestResolvePythonExecutableSkipsUnverifiableNames(t *testing.T) {
	// A name that is not on PATH at all must be skipped rather than returned.
	got := resolvePythonExecutable([]string{"definitely-not-a-python-abc123", "python", "python3"})
	if got == "definitely-not-a-python-abc123" {
		t.Fatalf("returned a name that is not on PATH: %q", got)
	}
}
