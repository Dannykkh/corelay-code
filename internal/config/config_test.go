package config

import (
	"path/filepath"
	"testing"
)

func TestConfigDirEnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", "")
	t.Setenv("ANICLEW_CONFIG_DIR", dir)

	want := filepath.Join(dir, "config.json")
	if got := ConfigPath(); got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestConfigDirCorelayEnvOverrideTakesPriority(t *testing.T) {
	current := t.TempDir()
	legacy := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", current)
	t.Setenv("ANICLEW_CONFIG_DIR", legacy)

	want := filepath.Join(current, "config.json")
	if got := ConfigPath(); got != want {
		t.Fatalf("ConfigPath() = %q, want %q", got, want)
	}
}

func TestConfigSaveLoadEvidencePolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", "")
	t.Setenv("ANICLEW_CONFIG_DIR", dir)

	cfg := DefaultConfig()
	cfg.EvidencePolicy = "block"
	cfg.EvidenceMaxStopBlocks = 4
	if err := Save(cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got := Load()
	if got.EvidencePolicy != "block" || got.EvidenceMaxStopBlocks != 4 {
		t.Fatalf("evidence policy = %q/%d, want block/4", got.EvidencePolicy, got.EvidenceMaxStopBlocks)
	}
}
