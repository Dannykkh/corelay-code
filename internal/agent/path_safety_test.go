package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func pathTestInput(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}
	return data
}

func pathTestWorkspace(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{workspace, outside, filepath.Join(workspace, "sub")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create test directory %s: %v", dir, err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(workspace, "inside.txt"):       "inside",
		filepath.Join(workspace, "sub", "inside.go"): "package inside",
		filepath.Join(outside, "secret.txt"):         "outside",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("create test file %s: %v", path, err)
		}
	}
	return workspace, outside
}

func TestCheckPathCanonicalizesExistingAndNewTargets(t *testing.T) {
	workspace, outside := pathTestWorkspace(t)
	cfg := PermissionConfig{}

	for _, path := range []string{
		"inside.txt",
		filepath.Join("new", "nested", "file.txt"),
		filepath.Join(workspace, "sub", "inside.go"),
	} {
		if ok, reason := CheckPath(path, workspace, cfg); !ok {
			t.Errorf("in-workspace path %q denied: %s", path, reason)
		}
	}

	for _, path := range []string{
		filepath.Join("..", "outside", "secret.txt"),
		filepath.Join(outside, "secret.txt"),
		filepath.Join(outside, "new", "file.txt"),
	} {
		if ok, _ := CheckPath(path, workspace, cfg); ok {
			t.Errorf("outside path %q was allowed", path)
		}
	}
}

func TestCheckPathRejectsSymlinkEscapesForExistingAndNewTargets(t *testing.T) {
	workspace, outside := pathTestWorkspace(t)
	link := filepath.Join(workspace, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	for _, path := range []string{
		filepath.Join("outside-link", "secret.txt"),
		filepath.Join("outside-link", "new", "file.txt"),
	} {
		if ok, _ := CheckPath(path, workspace, PermissionConfig{}); ok {
			t.Errorf("symlink escape %q was allowed", path)
		}
	}

	cfg := PermissionConfig{AutoApprove: "all"}
	toolCases := []struct {
		name  string
		input json.RawMessage
	}{
		{"LS", pathTestInput(t, map[string]any{"path": "outside-link"})},
		{"RepoMap", pathTestInput(t, map[string]any{"path": "outside-link"})},
		{"Grep", pathTestInput(t, map[string]any{"pattern": "secret", "path": "outside-link"})},
		{"Glob", pathTestInput(t, map[string]any{"pattern": filepath.Join("outside-link", "**", "*.txt")})},
	}
	for _, test := range toolCases {
		if allowed, _, _ := CheckPermission(test.name, test.input, workspace, cfg); allowed {
			t.Errorf("%s allowed a symlink escape", test.name)
		}
	}
}

func TestCheckPermissionCoversPathToolsAndExplicitAliases(t *testing.T) {
	workspace, outside := pathTestWorkspace(t)
	cfg := PermissionConfig{AutoApprove: "all"}
	outsidePath := filepath.Join(outside, "secret.txt")
	escapePath := filepath.Join("..", "outside", "secret.txt")

	denied := []struct {
		name  string
		input json.RawMessage
	}{
		{"Read", pathTestInput(t, map[string]any{"file_path": outsidePath})},
		{"Write", pathTestInput(t, map[string]any{"file_path": escapePath})},
		{"Edit", pathTestInput(t, map[string]any{"file_path": outsidePath})},
		{"LS", pathTestInput(t, map[string]any{"path": outside})},
		{"RepoMap", pathTestInput(t, map[string]any{"path": outside})},
		{"Grep", pathTestInput(t, map[string]any{"pattern": "secret", "path": outside})},
		{"Glob", pathTestInput(t, map[string]any{"pattern": filepath.Join("..", "outside", "*.txt")})},
		{"Glob", pathTestInput(t, map[string]any{"pattern": filepath.Join(outside, "*.txt")})},
		{"Glob", pathTestInput(t, map[string]any{"pattern": "*.txt", "path": outside})},
		{"read_file", pathTestInput(t, map[string]any{"file_path": outsidePath})},
		{"write_file", pathTestInput(t, map[string]any{"file_path": outsidePath})},
		{"edit_file", pathTestInput(t, map[string]any{"file_path": outsidePath})},
		{"list_files", pathTestInput(t, map[string]any{"path": outside})},
		{"search_files", pathTestInput(t, map[string]any{"pattern": "secret", "path": outside})},
		{"glob_files", pathTestInput(t, map[string]any{"pattern": "*.txt", "path": outside})},
	}
	for _, test := range denied {
		t.Run(test.name, func(t *testing.T) {
			allowed, reason, level := CheckPermission(test.name, test.input, workspace, cfg)
			if allowed {
				t.Fatalf("outside path was allowed")
			}
			if reason == "" {
				t.Fatal("denial did not include a reason")
			}
			if level != DangerDangerous {
				t.Fatalf("denied path danger = %q, want %q", level, DangerDangerous)
			}
		})
	}

	allowed := []struct {
		name  string
		input json.RawMessage
	}{
		{"Read", pathTestInput(t, map[string]any{"file_path": "inside.txt"})},
		{"Write", pathTestInput(t, map[string]any{"file_path": filepath.Join("new", "file.txt")})},
		{"Edit", pathTestInput(t, map[string]any{"file_path": "inside.txt"})},
		{"LS", pathTestInput(t, map[string]any{"path": "sub"})},
		{"LS", pathTestInput(t, map[string]any{})},
		{"RepoMap", pathTestInput(t, map[string]any{"path": "sub"})},
		{"RepoMap", pathTestInput(t, map[string]any{})},
		{"Grep", pathTestInput(t, map[string]any{"pattern": "package", "path": "sub", "glob": "*.go"})},
		{"Grep", pathTestInput(t, map[string]any{"pattern": "package"})},
		{"Glob", pathTestInput(t, map[string]any{"pattern": filepath.Join("sub", "**", "*.go")})},
	}
	for _, test := range allowed {
		t.Run("inside-"+test.name, func(t *testing.T) {
			if ok, reason, _ := CheckPermission(test.name, test.input, workspace, cfg); !ok {
				t.Fatalf("normal in-workspace path denied: %s", reason)
			}
		})
	}
}

func TestCheckPermissionRejectsMalformedAmbiguousAndUnsafeGlobInputs(t *testing.T) {
	workspace, _ := pathTestWorkspace(t)
	cfg := PermissionConfig{AutoApprove: "all"}

	tests := []struct {
		name  string
		tool  string
		input json.RawMessage
	}{
		{"malformed JSON", "Read", json.RawMessage(`{"file_path":`)},
		{"missing required path", "Read", json.RawMessage(`{}`)},
		{"duplicate path field", "Read", json.RawMessage(`{"file_path":"inside.txt","file_path":"../outside/secret.txt"}`)},
		{"conflicting path fields", "Read", json.RawMessage(`{"file_path":"inside.txt","path":"../outside/secret.txt"}`)},
		{"non-string grep path", "Grep", json.RawMessage(`{"pattern":"x","path":42}`)},
		{"malformed glob", "Glob", json.RawMessage(`{"pattern":"["}`)},
		{"post-wildcard traversal", "Glob", json.RawMessage(`{"pattern":"sub*/../../outside/*.txt"}`)},
		{"trailing JSON", "LS", json.RawMessage(`{} {}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if allowed, _, _ := CheckPermission(test.tool, test.input, workspace, cfg); allowed {
				t.Fatal("unsafe or ambiguous input was allowed")
			}
		})
	}
}

func TestCheckPathFailsClosedForUnresolvedWorkspace(t *testing.T) {
	missingWorkspace := filepath.Join(t.TempDir(), "missing-workspace")
	if ok, _ := CheckPath("new.txt", missingWorkspace, PermissionConfig{}); ok {
		t.Fatal("path under an unresolved workspace was allowed")
	}
}

func TestPathWithinWindowsVolumeUNCAndCaseSemantics(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path semantics")
	}

	if !pathWithin(`c:\work\project\sub\file.txt`, `C:\Work\Project`) {
		t.Fatal("same-volume path with different case was rejected")
	}
	if pathWithin(`D:\Work\Project\file.txt`, `C:\Work\Project`) {
		t.Fatal("different-drive path was accepted")
	}
	if !pathWithin(`\\server\share\work\sub\file.txt`, `\\SERVER\SHARE\Work`) {
		t.Fatal("same UNC share with different case was rejected")
	}
	if pathWithin(`\\server\other\work\file.txt`, `\\server\share\work`) {
		t.Fatal("different UNC share was accepted")
	}
}

func TestCheckPathRejectsAmbiguousWindowsForms(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path semantics")
	}
	workspace, _ := pathTestWorkspace(t)
	for _, path := range []string{
		`C:relative.txt`,
		`\root-relative.txt`,
		`\\?\C:\device.txt`,
		`inside.txt:stream`,
		`NUL.txt`,
		`sub\CON.log`,
		`trailing.`,
		`trailing `,
	} {
		if ok, _ := CheckPath(path, workspace, PermissionConfig{}); ok {
			t.Errorf("ambiguous Windows path %q was allowed", path)
		}
	}
}
