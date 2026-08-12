package config

import (
	"path/filepath"
	"testing"
)

func TestCapabilityProfileDirFollowsConfiguredBaseDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", "")
	t.Setenv("CORELAY_CONFIG_DIR", base)
	want := filepath.Join(base, "capability-profiles")
	if got := CapabilityProfileDir(); got != want {
		t.Fatalf("CapabilityProfileDir() = %q, want %q", got, want)
	}
}
