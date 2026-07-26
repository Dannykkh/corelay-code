package config

import (
	"os"
	"path/filepath"
	"testing"
)

// withHome points UserHomeDir at a temp dir for the duration of a test.
func withHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	if os.PathSeparator == '\\' {
		t.Setenv("USERPROFILE", home)
	} else {
		t.Setenv("HOME", home)
	}
	return home
}

func TestBaseDirUsesNewNameOnCleanMachine(t *testing.T) {
	home := withHome(t)

	want := filepath.Join(home, BaseDirName)
	if got := BaseDir(); got != want {
		t.Fatalf("BaseDir() = %q, want %q", got, want)
	}
}

func TestBaseDirKeepsExistingLegacyDirectory(t *testing.T) {
	// The rename must not strand an existing install: receipts, undo snapshots
	// and per-project memory all live under this path.
	home := withHome(t)
	legacy := filepath.Join(home, LegacyBaseDirName)
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := BaseDir(); got != legacy {
		t.Fatalf("BaseDir() = %q, want the existing legacy dir %q", got, legacy)
	}
}

func TestBaseDirPrefersNewNameWhenBothExist(t *testing.T) {
	home := withHome(t)
	for _, name := range []string{BaseDirName, LegacyBaseDirName} {
		if err := os.MkdirAll(filepath.Join(home, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	want := filepath.Join(home, BaseDirName)
	if got := BaseDir(); got != want {
		t.Fatalf("BaseDir() = %q, want %q", got, want)
	}
}

func TestBaseDirHonoursExplicitOverride(t *testing.T) {
	home := withHome(t)
	if err := os.MkdirAll(filepath.Join(home, LegacyBaseDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	override := t.TempDir()
	t.Setenv("ANICLEW_CONFIG_DIR", override)

	if got := BaseDir(); got != override {
		t.Fatalf("BaseDir() = %q, want the override %q", got, override)
	}
}

func TestIsBaseDirPathMatchesBothNames(t *testing.T) {
	cases := map[string]bool{
		filepath.Join("home", "u", BaseDirName, "receipts", "x.json"):  true,
		filepath.Join("home", "u", LegacyBaseDirName, "undo", "abc"):   true,
		filepath.Join("home", "u", "projects", "somewhere", "file.go"): false,
		filepath.Join("etc", "passwd"):                                 false,
	}
	for path, want := range cases {
		if got := IsBaseDirPath(path); got != want {
			t.Errorf("IsBaseDirPath(%q) = %v, want %v", path, got, want)
		}
	}
}
