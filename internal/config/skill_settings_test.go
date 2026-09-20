package config

import (
	"reflect"
	"testing"
)

func TestResolveSkillSettingsUsesExactProjectWorkspaceOverrides(t *testing.T) {
	globalWorkspace := t.TempDir()
	projectA := t.TempDir()
	projectB := t.TempDir()
	cfg := DefaultConfig()
	cfg.SkillSource = " ALL "
	cfg.SkillDirs = []string{"global skills", "  "}
	cfg.Projects = []Project{
		{Path: projectA, Name: "A", SkillSource: " none ", SkillDirs: []string{"project A skills"}},
		{Path: projectB, Name: "B", SkillSource: "codex", SkillDirs: []string{"project B skills"}},
	}

	settingsA, err := ResolveSkillSettings(cfg, projectA)
	if err != nil {
		t.Fatalf("resolve project A settings: %v", err)
	}
	if settingsA.Source != "none" || !reflect.DeepEqual(settingsA.Dirs, []string{"global skills"}) ||
		!reflect.DeepEqual(settingsA.ProjectDirs, []string{"project A skills"}) {
		t.Fatalf("project A settings = %+v", settingsA)
	}

	settingsB, err := ResolveSkillSettings(cfg, projectB)
	if err != nil {
		t.Fatalf("resolve project B settings: %v", err)
	}
	if settingsB.Source != "codex" || !reflect.DeepEqual(settingsB.Dirs, []string{"global skills"}) ||
		!reflect.DeepEqual(settingsB.ProjectDirs, []string{"project B skills"}) {
		t.Fatalf("project B settings = %+v", settingsB)
	}

	settingsGlobal, err := ResolveSkillSettings(cfg, globalWorkspace)
	if err != nil {
		t.Fatalf("resolve unregistered workspace settings: %v", err)
	}
	if settingsGlobal.Source != "all" || !reflect.DeepEqual(settingsGlobal.Dirs, []string{"global skills"}) || len(settingsGlobal.ProjectDirs) != 0 {
		t.Fatalf("unregistered workspace inherited project settings: %+v", settingsGlobal)
	}
}

func TestResolveSkillSettingsFailsClosedForInvalidEffectiveSource(t *testing.T) {
	workspace := t.TempDir()
	cfg := DefaultConfig()
	cfg.SkillSource = "invalid"
	cfg.Projects = []Project{{Path: workspace, Name: "valid override", SkillSource: "claude"}}

	if _, err := ResolveSkillSettings(cfg, workspace); err != nil {
		t.Fatalf("valid project override should resolve despite invalid unused global: %v", err)
	}
	if _, err := ResolveSkillSettings(cfg, t.TempDir()); err == nil {
		t.Fatal("invalid effective global source should fail closed")
	}
}
