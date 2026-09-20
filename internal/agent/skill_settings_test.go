package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/types"
)

func TestLoadSkillsNoneDisablesProjectAndCustomRoots(t *testing.T) {
	workDir := t.TempDir()
	customDir := filepath.Join(t.TempDir(), "custom skills 한글")
	writeSkillFixture(t, filepath.Join(workDir, ".claude", "skills"), "project-skill", "project body")
	writeSkillFixture(t, customDir, "custom-skill", "custom body")

	got := LoadSkillsWithSource(workDir, "none", []string{customDir})
	if len(got) != 0 {
		t.Fatalf("source none loaded project/custom skills: %+v", got)
	}
}

func TestLoadSkillsUsesProjectRootOrderAndNamespacesShadowedCopies(t *testing.T) {
	isolateSkillHome(t)
	workDir := t.TempDir()
	projectCustom := filepath.Join(t.TempDir(), "project-custom")
	globalCustom := filepath.Join(t.TempDir(), ".claude-custom-root")
	writeSkillFixture(t, filepath.Join(workDir, ".claude", "skills"), "Shared", "PROJECT_CLAUDE_BODY")
	writeSkillFixture(t, filepath.Join(workDir, ".codex", "skills"), "shared", "PROJECT_CODEX_BODY")
	writeSkillFixture(t, filepath.Join(workDir, ".agents", "skills"), "agents-only", "PROJECT_AGENTS_BODY")
	writeSkillFixture(t, projectCustom, "shared", "PROJECT_CUSTOM_BODY")
	writeSkillFixture(t, globalCustom, "shared", "GLOBAL_CUSTOM_BODY")

	skills := LoadSkillsWithRoots(workDir, "all", []string{projectCustom}, []string{globalCustom})
	if len(skills) != 2 {
		t.Fatalf("skills = %+v, want first-wins shared and agents-only", skills)
	}
	if skills[0].Name != "Shared" || skills[0].Source != "claude" || skills[0].Namespace != "project-claude" ||
		!strings.Contains(skills[0].Content, "PROJECT_CLAUDE_BODY") {
		t.Fatalf("first root did not win the case-insensitive collision: %+v", skills[0])
	}
	if len(skills[0].Shadowed) != 3 {
		t.Fatalf("shadowed origins = %+v, want project codex, project custom, and global custom", skills[0].Shadowed)
	}
	wantNamespaces := []string{"project-codex", "project-custom-1", "custom-1"}
	for i, want := range wantNamespaces {
		if skills[0].Shadowed[i].Namespace != want {
			t.Fatalf("shadowed[%d] namespace = %q, want %q", i, skills[0].Shadowed[i].Namespace, want)
		}
	}
	if skills[1].Name != "agents-only" || skills[1].Source != "agents" || skills[1].Namespace != "project-agents" {
		t.Fatalf("project .agents skill metadata = %+v", skills[1])
	}

	commands := ParseSlashCommands(skills)
	commandNames := make([]string, 0, len(commands))
	for _, command := range commands {
		commandNames = append(commandNames, command.Name)
	}
	if !skillTestContains(commandNames, "Shared") || !skillTestContains(commandNames, "project-claude/Shared") ||
		!skillTestContains(commandNames, "project-codex/shared") || !skillTestContains(commandNames, "project-custom-1/shared") ||
		!skillTestContains(commandNames, "custom-1/shared") {
		t.Fatalf("slash commands do not expose bare and namespaced choices: %v", commandNames)
	}
	bare, err := ProcessSlashCommand("/shared", skills)
	if err != nil || !strings.Contains(bare, "PROJECT_CLAUDE_BODY") {
		t.Fatalf("bare first-wins command = %q, err=%v", bare, err)
	}
	namespaced, err := ProcessSlashCommand("/project-codex/shared use codex", skills)
	if err != nil || !strings.Contains(namespaced, "PROJECT_CODEX_BODY") || !strings.Contains(namespaced, "User arguments: use codex") {
		t.Fatalf("namespaced command = %q, err=%v", namespaced, err)
	}
}

func TestLoadSkillsGroupsUnicodeSimpleFoldCollisionsForSlashResolution(t *testing.T) {
	isolateSkillHome(t)
	workDir := t.TempDir()
	writeSkillFixture(t, filepath.Join(workDir, ".claude", "skills"), "S", "LATIN_CAPITAL_S")
	writeSkillFixture(t, filepath.Join(workDir, ".codex", "skills"), "\u017f", "LATIN_SMALL_LONG_S")

	skills := LoadSkillsWithRoots(workDir, "all", nil, nil)
	if len(skills) != 1 || skills[0].Name != "S" || len(skills[0].Shadowed) != 1 {
		t.Fatalf("Unicode case-fold collision was not reported as first-wins: %+v", skills)
	}
	commands := ParseSlashCommands(skills)
	commandNames := make([]string, 0, len(commands))
	for _, command := range commands {
		commandNames = append(commandNames, command.Name)
	}
	if !skillTestContains(commandNames, "project-codex/\u017f") {
		t.Fatalf("namespaced command for shadowed Unicode skill missing: %v", commandNames)
	}
	selected, err := ProcessSlashCommand("/project-codex/\u017f", skills)
	if err != nil || !strings.Contains(selected, "LATIN_SMALL_LONG_S") {
		t.Fatalf("namespaced Unicode command selected %q, err=%v", selected, err)
	}
}

func TestLoadSkillsProjectVendorRootsAreSourceIndependentButHomeRootsAreFiltered(t *testing.T) {
	isolateSkillHome(t)
	workDir := t.TempDir()
	writeSkillFixture(t, filepath.Join(workDir, ".claude", "skills"), "project-claude", "PROJECT_CLAUDE")
	writeSkillFixture(t, filepath.Join(workDir, ".codex", "skills"), "project-codex", "PROJECT_CODEX")
	writeSkillFixture(t, filepath.Join(workDir, ".agents", "skills"), "project-agents", "PROJECT_AGENTS")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	writeSkillFixture(t, filepath.Join(home, ".claude", "skills"), "user-claude", "USER_CLAUDE")
	writeSkillFixture(t, filepath.Join(home, ".codex", "skills"), "user-codex", "USER_CODEX")
	writeSkillFixture(t, filepath.Join(home, ".gemini", "skills"), "user-gemini", "USER_GEMINI")

	codex := LoadSkillsWithSource(workDir, "codex", nil)
	got := make([]string, 0, len(codex))
	for _, skill := range codex {
		got = append(got, skill.Name)
	}
	for _, want := range []string{"project-claude", "project-codex", "project-agents", "user-codex"} {
		if !skillTestContains(got, want) {
			t.Errorf("codex source omitted %q: got %v", want, got)
		}
	}
	for _, unwanted := range []string{"user-claude", "user-gemini"} {
		if skillTestContains(got, unwanted) {
			t.Errorf("codex source unexpectedly loaded %q: got %v", unwanted, got)
		}
	}
	if none := LoadSkillsWithSource(workDir, "none", nil); len(none) != 0 {
		t.Fatalf("none source loaded vendor roots: %+v", none)
	}
}

func TestRunLoopNoneDisablesSkillSlashAndNaturalLanguageDiscovery(t *testing.T) {
	isolateEvidenceLoopTest(t)
	isolateSkillHome(t)
	workDir := t.TempDir()
	skillRoot := filepath.Join(workDir, ".claude", "skills")
	writeSkillFixture(t, skillRoot, "hidden-skill", "HIDDEN_SKILL_BODY")
	skillReferences := filepath.Join(skillRoot, "hidden-skill", "references")
	if err := os.MkdirAll(skillReferences, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillReferences, "crawler.md"), []byte("DISABLED_SKILL_REFERENCE_BODY hidden reference"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := phase2Profile("skill-none", 32_768, 4_096)

	t.Run("slash skill is unavailable", func(t *testing.T) {
		provider := &phase2NoCallProvider{}
		events := runSkillPolicyLoop(t, provider, workDir, "/hidden-skill", RunOptions{
			HarnessProfile:      &profile,
			SkillSource:         "none",
			DisablePlugins:      true,
			DisableWorkspaceMCP: true,
		})
		foundUnknown := false
		for _, event := range events {
			if event.Type == "error" && strings.Contains(fmt.Sprint(event.Data), "Unknown command: /hidden-skill") {
				foundUnknown = true
			}
		}
		if !foundUnknown || provider.Calls() != 0 {
			t.Fatalf("none should reject project skill before provider call; unknown=%v calls=%d events=%+v", foundUnknown, provider.Calls(), events)
		}
	})

	t.Run("natural language catalog is absent", func(t *testing.T) {
		provider := &completionLoopProvider{steps: []completionLoopStep{
			func(request *types.MessagesRequest) (scriptedLoopStep, error) {
				system := completionRequestSystemText(request)
				if strings.Contains(system, "## Skills") || strings.Contains(system, "HIDDEN_SKILL_BODY") || strings.Contains(system, "DISABLED_SKILL_REFERENCE_BODY") {
					return scriptedLoopStep{}, fmt.Errorf("disabled skill was advertised in system prompt")
				}
				return textStep("completed without skill discovery"), nil
			},
		}}
		events := runSkillPolicyLoop(t, provider, workDir, "explain hidden project reference", RunOptions{
			HarnessProfile:      &profile,
			SkillSource:         "none",
			DisablePlugins:      true,
			DisableWorkspaceMCP: true,
		})
		requests, errs := provider.snapshot()
		if len(errs) != 0 || len(requests) != 1 {
			t.Fatalf("provider request count/errors = %d/%v; events=%+v", len(requests), errs, events)
		}
	})
}

func TestRunLoopLoadsSlashSkillFromConfiguredExtraDirectory(t *testing.T) {
	isolateEvidenceLoopTest(t)
	isolateSkillHome(t)
	workDir := t.TempDir()
	extraDir := filepath.Join(t.TempDir(), "custom skills 한글")
	writeSkillFixture(t, extraDir, "extra-skill", "EXTRA_SKILL_BODY_FROM_CONFIGURED_ROOT")
	profile := phase2Profile("skill-extra-root", 32_768, 4_096)
	provider := &completionLoopProvider{steps: []completionLoopStep{
		func(request *types.MessagesRequest) (scriptedLoopStep, error) {
			if !strings.Contains(string(mustJSON(request.Messages)), "EXTRA_SKILL_BODY_FROM_CONFIGURED_ROOT") {
				return scriptedLoopStep{}, fmt.Errorf("configured skill body did not reach the request")
			}
			return textStep("custom skill completed"), nil
		},
	}}
	events := runSkillPolicyLoop(t, provider, workDir, "/extra-skill", RunOptions{
		HarnessProfile:      &profile,
		SkillSource:         "claude",
		SkillDirs:           []string{extraDir},
		DisablePlugins:      true,
		DisableWorkspaceMCP: true,
	})
	requests, errs := provider.snapshot()
	if len(errs) != 0 || len(requests) != 1 {
		t.Fatalf("provider request count/errors = %d/%v; events=%+v", len(requests), errs, events)
	}
	if !strings.Contains(string(mustJSON(requests[0].Messages)), "EXTRA_SKILL_BODY_FROM_CONFIGURED_ROOT") {
		t.Fatal("selected skill body was not loaded for slash command execution")
	}
}

func TestRunLoopResolvesSkillPolicyFromProjectWorkspace(t *testing.T) {
	isolateEvidenceLoopTest(t)
	isolateSkillHome(t)
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	workDir := t.TempDir()
	writeSkillFixture(t, filepath.Join(workDir, ".claude", "skills"), "workspace-hidden", "WORKSPACE_HIDDEN_BODY")
	cfg := config.DefaultConfig()
	cfg.SkillSource = "all"
	cfg.Projects = []config.Project{{Path: workDir, Name: "isolated", SkillSource: "none"}}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save test project skill policy: %v", err)
	}
	profile := phase2Profile("project-skill-none", 32_768, 4_096)
	provider := &phase2NoCallProvider{}
	events := runSkillPolicyLoop(t, provider, workDir, "/workspace-hidden", RunOptions{
		HarnessProfile:      &profile,
		DisablePlugins:      true,
		DisableWorkspaceMCP: true,
	})
	foundUnknown := false
	for _, event := range events {
		if event.Type == "error" && strings.Contains(fmt.Sprint(event.Data), "Unknown command: /workspace-hidden") {
			foundUnknown = true
		}
	}
	if !foundUnknown || provider.Calls() != 0 {
		t.Fatalf("project none policy should reject skill before provider call; unknown=%v calls=%d events=%+v", foundUnknown, provider.Calls(), events)
	}
}

func TestRunLoopFailsClosedWhenSkillPolicyConfigCannotBeRead(t *testing.T) {
	isolateEvidenceLoopTest(t)
	isolateSkillHome(t)
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write invalid config fixture: %v", err)
	}
	workDir := t.TempDir()
	provider := &phase2NoCallProvider{}
	profile := phase2Profile("invalid-skill-config", 32_768, 4_096)
	events := runSkillPolicyLoop(t, provider, workDir, "answer this", RunOptions{
		HarnessProfile:      &profile,
		DisablePlugins:      true,
		DisableWorkspaceMCP: true,
	})
	if provider.Calls() != 0 {
		t.Fatalf("invalid skill policy config reached provider %d times; events=%+v", provider.Calls(), events)
	}
	foundInvalidConfig := false
	for _, event := range events {
		if event.Type == "error" && strings.Contains(fmt.Sprint(event.Data), "cannot load workspace skill policy") {
			foundInvalidConfig = true
		}
	}
	if !foundInvalidConfig {
		encoded, _ := json.Marshal(events)
		t.Fatalf("run did not fail closed for invalid config; events=%s", encoded)
	}
}

func runSkillPolicyLoop(t *testing.T, provider types.Provider, workDir, prompt string, opts RunOptions) []Event {
	t.Helper()
	events := make(chan Event, 128)
	go RunLoopWithOptions(context.Background(), provider, "skill-policy-model", []types.Message{{
		Role: "user", Content: mustJSON(prompt),
	}}, workDir, opts, events)
	var result []Event
	for event := range events {
		result = append(result, event)
	}
	return result
}

func writeSkillFixture(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: fixture\n---\n\n"+content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func isolateSkillHome(t *testing.T) {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("USERPROFILE", homeDir)
	t.Setenv("HOME", homeDir)
}

func skillTestContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
