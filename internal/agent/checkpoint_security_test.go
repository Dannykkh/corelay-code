package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/types"
)

type checkpointSecurityFixture struct {
	root      string
	workDir   string
	stateDir  string
	outside   string
	canonical string
	dir       string
}

func newCheckpointSecurityFixture(t *testing.T) checkpointSecurityFixture {
	t.Helper()
	root := t.TempDir()
	fixture := checkpointSecurityFixture{
		root:     root,
		workDir:  filepath.Join(root, "workspace"),
		stateDir: filepath.Join(root, "state"),
		outside:  filepath.Join(root, "outside"),
	}
	t.Setenv("ANICLEW_CONFIG_DIR", fixture.stateDir)
	for _, dir := range []string{fixture.workDir, fixture.stateDir, fixture.outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create fixture directory %s: %v", dir, err)
		}
	}
	canonical, err := canonicalWorkspace(fixture.workDir)
	if err != nil {
		t.Fatalf("canonical workspace: %v", err)
	}
	fixture.canonical = canonical
	fixture.dir = checkpointDir(fixture.workDir)
	if fixture.dir == "" {
		t.Fatal("checkpointDir returned an empty path")
	}
	if err := os.MkdirAll(fixture.dir, 0o755); err != nil {
		t.Fatalf("create checkpoint directory: %v", err)
	}
	return fixture
}

func writeCheckpointSecurityFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeCheckpointSecurityManifest(t *testing.T, fixture checkpointSecurityFixture, m ckptManifest) {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixture.dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func validCheckpointSecurityManifest(fixture checkpointSecurityFixture, files map[string]ckptFile) ckptManifest {
	return ckptManifest{
		Version: checkpointManifestVersion,
		WorkDir: fixture.canonical,
		Files:   files,
	}
}

func TestUndoCheckpointSecureRestoresValidManifest(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	existing := filepath.Join(fixture.workDir, "existing.txt")
	created := filepath.Join(fixture.workDir, "created.txt")
	writeCheckpointSecurityFile(t, existing, "before")

	startCheckpoint(fixture.workDir)
	checkpointFile(fixture.workDir, "existing.txt", existing)
	writeCheckpointSecurityFile(t, existing, "after")
	checkpointFile(fixture.workDir, "created.txt", created)
	writeCheckpointSecurityFile(t, created, "created")

	reverted, ok, err := undoCheckpointSecure(fixture.workDir)
	if err != nil {
		t.Fatalf("undoCheckpointSecure: %v", err)
	}
	if !ok || len(reverted) != 2 {
		t.Fatalf("reverted = %#v, ok = %v; want two entries", reverted, ok)
	}
	if data, err := os.ReadFile(existing); err != nil || string(data) != "before" {
		t.Fatalf("existing file = %q, err = %v; want before", data, err)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Fatalf("created file still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(fixture.dir); !os.IsNotExist(err) {
		t.Fatalf("checkpoint directory was not cleared: %v", err)
	}
}

func TestUndoCheckpointPreflightsAllEntriesBeforeMutation(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	createdCanary := filepath.Join(fixture.workDir, "a-created-canary.txt")
	outsideCanary := filepath.Join(fixture.outside, "outside-canary.txt")
	writeCheckpointSecurityFile(t, createdCanary, "must remain")
	writeCheckpointSecurityFile(t, outsideCanary, "outside must remain")
	writeCheckpointSecurityManifest(t, fixture, validCheckpointSecurityManifest(fixture, map[string]ckptFile{
		"a-created-canary.txt":       {Existed: false},
		"z/../../outside-canary.txt": {Existed: false},
	}))

	if reverted, ok, err := undoCheckpointSecure(fixture.workDir); err == nil || ok || len(reverted) != 0 {
		t.Fatalf("unsafe manifest result = %#v, %v, %v; want rejected before mutation", reverted, ok, err)
	}
	if data, err := os.ReadFile(createdCanary); err != nil || string(data) != "must remain" {
		t.Fatalf("valid first entry was partially applied: %q, %v", data, err)
	}
	if data, err := os.ReadFile(outsideCanary); err != nil || string(data) != "outside must remain" {
		t.Fatalf("outside canary changed: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.dir, "manifest.json")); err != nil {
		t.Fatalf("rejected manifest should remain for diagnosis: %v", err)
	}
}

func TestUndoCheckpointClearsValidExhaustedCreatedEntry(t *testing.T) {
	fixture := newCheckpointSecurityFixture(t)
	writeCheckpointSecurityManifest(t, fixture, validCheckpointSecurityManifest(fixture, map[string]ckptFile{
		"already-absent.txt": {Existed: false},
	}))

	if reverted, ok, err := undoCheckpointSecure(fixture.workDir); err != nil || ok || len(reverted) != 0 {
		t.Fatalf("exhausted manifest result = %#v, %v, %v", reverted, ok, err)
	}
	if _, err := os.Stat(fixture.dir); !os.IsNotExist(err) {
		t.Fatalf("exhausted checkpoint was not cleared: %v", err)
	}
}

func TestUndoCheckpointRejectsUnsafeManifestTargets(t *testing.T) {
	tests := []struct {
		name   string
		target func(checkpointSecurityFixture) string
	}{
		{"parent traversal", func(checkpointSecurityFixture) string { return "../outside/canary.txt" }},
		{"unclean traversal", func(checkpointSecurityFixture) string { return "safe/../canary.txt" }},
		{"absolute outside", func(f checkpointSecurityFixture) string { return filepath.Join(f.outside, "canary.txt") }},
		{"windows drive", func(checkpointSecurityFixture) string { return `C:\outside\canary.txt` }},
		{"windows UNC", func(checkpointSecurityFixture) string { return `\\server\share\canary.txt` }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCheckpointSecurityFixture(t)
			canary := filepath.Join(fixture.outside, "canary.txt")
			writeCheckpointSecurityFile(t, canary, "unchanged")
			writeCheckpointSecurityManifest(t, fixture, validCheckpointSecurityManifest(fixture, map[string]ckptFile{
				test.target(fixture): {Existed: false},
			}))

			if _, ok, err := undoCheckpointSecure(fixture.workDir); err == nil || ok {
				t.Fatalf("unsafe target was not rejected: ok=%v err=%v", ok, err)
			}
			if data, err := os.ReadFile(canary); err != nil || string(data) != "unchanged" {
				t.Fatalf("outside canary changed: %q, %v", data, err)
			}
		})
	}
}

func TestUndoCheckpointRejectsTargetAndBackupSymlinkEscapes(t *testing.T) {
	t.Run("target", func(t *testing.T) {
		fixture := newCheckpointSecurityFixture(t)
		outsideCanary := filepath.Join(fixture.outside, "canary.txt")
		writeCheckpointSecurityFile(t, outsideCanary, "outside")
		if err := os.Symlink(fixture.outside, filepath.Join(fixture.workDir, "escape")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		writeCheckpointSecurityManifest(t, fixture, validCheckpointSecurityManifest(fixture, map[string]ckptFile{
			filepath.Join("escape", "canary.txt"): {Existed: false},
		}))

		if _, ok, err := undoCheckpointSecure(fixture.workDir); err == nil || ok {
			t.Fatalf("target symlink escape was not rejected: ok=%v err=%v", ok, err)
		}
		if data, err := os.ReadFile(outsideCanary); err != nil || string(data) != "outside" {
			t.Fatalf("outside target changed: %q, %v", data, err)
		}
	})

	t.Run("backup", func(t *testing.T) {
		fixture := newCheckpointSecurityFixture(t)
		victim := filepath.Join(fixture.workDir, "victim.txt")
		outsideBackup := filepath.Join(fixture.outside, "outside.bak")
		writeCheckpointSecurityFile(t, victim, "current")
		writeCheckpointSecurityFile(t, outsideBackup, "malicious backup")
		if err := os.Symlink(outsideBackup, filepath.Join(fixture.dir, "escape.bak")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		writeCheckpointSecurityManifest(t, fixture, validCheckpointSecurityManifest(fixture, map[string]ckptFile{
			"victim.txt": {Existed: true, Backup: "escape.bak"},
		}))

		if _, ok, err := undoCheckpointSecure(fixture.workDir); err == nil || ok {
			t.Fatalf("backup symlink escape was not rejected: ok=%v err=%v", ok, err)
		}
		if data, err := os.ReadFile(victim); err != nil || string(data) != "current" {
			t.Fatalf("victim changed: %q, %v", data, err)
		}
	})
}

func TestUndoCheckpointRejectsUnsafeBackupPathsBeforeMutation(t *testing.T) {
	tests := []struct {
		name   string
		backup func(checkpointSecurityFixture) string
	}{
		{"parent traversal", func(checkpointSecurityFixture) string { return "../outside.bak" }},
		{"unclean traversal", func(checkpointSecurityFixture) string { return "safe/../outside.bak" }},
		{"absolute outside", func(f checkpointSecurityFixture) string { return filepath.Join(f.outside, "outside.bak") }},
		{"windows drive", func(checkpointSecurityFixture) string { return `C:\outside\outside.bak` }},
		{"windows UNC", func(checkpointSecurityFixture) string { return `\\server\share\outside.bak` }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCheckpointSecurityFixture(t)
			victim := filepath.Join(fixture.workDir, "victim.txt")
			writeCheckpointSecurityFile(t, victim, "current")
			writeCheckpointSecurityManifest(t, fixture, validCheckpointSecurityManifest(fixture, map[string]ckptFile{
				"victim.txt": {Existed: true, Backup: test.backup(fixture)},
			}))

			if _, ok, err := undoCheckpointSecure(fixture.workDir); err == nil || ok {
				t.Fatalf("unsafe backup was not rejected: ok=%v err=%v", ok, err)
			}
			if data, err := os.ReadFile(victim); err != nil || string(data) != "current" {
				t.Fatalf("victim changed before full preflight: %q, %v", data, err)
			}
		})
	}
}

func TestUndoCheckpointRejectsVersionWorkspaceAndControlState(t *testing.T) {
	t.Run("version", func(t *testing.T) {
		fixture := newCheckpointSecurityFixture(t)
		canary := filepath.Join(fixture.workDir, "canary.txt")
		writeCheckpointSecurityFile(t, canary, "keep")
		m := validCheckpointSecurityManifest(fixture, map[string]ckptFile{"canary.txt": {Existed: false}})
		m.Version++
		writeCheckpointSecurityManifest(t, fixture, m)
		if _, ok, err := undoCheckpointSecure(fixture.workDir); err == nil || ok {
			t.Fatalf("unsupported version was not rejected: ok=%v err=%v", ok, err)
		}
		if data, _ := os.ReadFile(canary); string(data) != "keep" {
			t.Fatalf("version rejection changed canary: %q", data)
		}
	})

	t.Run("workspace binding", func(t *testing.T) {
		fixture := newCheckpointSecurityFixture(t)
		canary := filepath.Join(fixture.workDir, "canary.txt")
		otherWork := filepath.Join(fixture.root, "other-workspace")
		writeCheckpointSecurityFile(t, canary, "keep")
		if err := os.MkdirAll(otherWork, 0o755); err != nil {
			t.Fatal(err)
		}
		otherCanonical, err := canonicalWorkspace(otherWork)
		if err != nil {
			t.Fatal(err)
		}
		m := validCheckpointSecurityManifest(fixture, map[string]ckptFile{"canary.txt": {Existed: false}})
		m.WorkDir = otherCanonical
		writeCheckpointSecurityManifest(t, fixture, m)
		if _, ok, err := undoCheckpointSecure(fixture.workDir); err == nil || ok {
			t.Fatalf("foreign workspace manifest was not rejected: ok=%v err=%v", ok, err)
		}
		if data, _ := os.ReadFile(canary); string(data) != "keep" {
			t.Fatalf("workspace rejection changed canary: %q", data)
		}
	})

	t.Run("checkpoint control backup", func(t *testing.T) {
		fixture := newCheckpointSecurityFixture(t)
		victim := filepath.Join(fixture.workDir, "victim.txt")
		writeCheckpointSecurityFile(t, victim, "current")
		writeCheckpointSecurityManifest(t, fixture, validCheckpointSecurityManifest(fixture, map[string]ckptFile{
			"victim.txt": {Existed: true, Backup: "manifest.json"},
		}))
		if _, ok, err := undoCheckpointSecure(fixture.workDir); err == nil || ok {
			t.Fatalf("manifest-as-backup was not rejected: ok=%v err=%v", ok, err)
		}
		if data, _ := os.ReadFile(victim); string(data) != "current" {
			t.Fatalf("control-path rejection changed victim: %q", data)
		}
	})
}

func TestUndoCheckpointRejectsAgentAndRepositoryControlTargets(t *testing.T) {
	tests := []string{".aniclew", ".git"}
	for _, controlDir := range tests {
		t.Run(controlDir, func(t *testing.T) {
			fixture := newCheckpointSecurityFixture(t)
			canaryRel := filepath.Join(controlDir, "canary.txt")
			canary := filepath.Join(fixture.workDir, canaryRel)
			writeCheckpointSecurityFile(t, canary, "keep")
			writeCheckpointSecurityManifest(t, fixture, validCheckpointSecurityManifest(fixture, map[string]ckptFile{
				canaryRel: {Existed: false},
			}))
			if _, ok, err := undoCheckpointSecure(fixture.workDir); err == nil || ok {
				t.Fatalf("control target was not rejected: ok=%v err=%v", ok, err)
			}
			if data, _ := os.ReadFile(canary); string(data) != "keep" {
				t.Fatalf("control canary changed: %q", data)
			}
		})
	}
}

func TestExplicitUndoRequestRequiresTopLevelUserConsent(t *testing.T) {
	stringMessage := func(role, text string) types.Message {
		data, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		return types.Message{Role: role, Content: data}
	}
	tests := []struct {
		name     string
		messages []types.Message
		opts     RunOptions
		want     bool
	}{
		{"exact user command", []types.Message{stringMessage("user", " /UNDO ")}, RunOptions{}, true},
		{"assistant cannot consent", []types.Message{stringMessage("assistant", "/undo")}, RunOptions{}, false},
		{"worker prompt is not user consent", []types.Message{stringMessage("user", "/undo")}, RunOptions{WorkerID: "worker-model-task"}, false},
		{"ordinary user text", []types.Message{stringMessage("user", "/undo please")}, RunOptions{}, false},
		{"tool result object", []types.Message{{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","content":"/undo"}]`)}}, RunOptions{}, false},
		{"no messages", nil, RunOptions{}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isExplicitUndoRequest(test.messages, test.opts); got != test.want {
				t.Fatalf("isExplicitUndoRequest() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCleanCheckpointRelativeRejectsPortableEscapes(t *testing.T) {
	unsafe := []string{
		"",
		"..",
		"../outside",
		`..\outside`,
		"safe/../outside",
		"/absolute",
		`C:\absolute`,
		`C:drive-relative`,
		`\\server\share\file`,
		"double//separator",
		"trailing/",
	}
	for _, path := range unsafe {
		if _, err := cleanCheckpointRelative(path); err == nil {
			t.Errorf("unsafe relative path %q was accepted", path)
		}
	}
	for _, path := range []string{"file.txt", filepath.Join("nested", "file.txt")} {
		if cleaned, err := cleanCheckpointRelative(path); err != nil || cleaned != path {
			t.Errorf("clean relative path %q = %q, %v", path, cleaned, err)
		}
	}
}
