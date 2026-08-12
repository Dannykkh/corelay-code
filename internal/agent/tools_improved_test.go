package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func editInput(t *testing.T, path, old, newStr string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"file_path":  path,
		"old_string": old,
		"new_string": newStr,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// A valid edit applies normally and passes the lint gate.
func TestExecuteEditV2_ValidEdit(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "main.go")
	os.WriteFile(f, []byte("package main\n\nfunc main() {\n\tx := 1\n\t_ = x\n}\n"), 0644)

	out, isErr := executeEditV2WithOptions(
		editInput(t, "main.go", "x := 1", "x := 42"),
		dir,
		secureGoLintFixtureOptions(t),
	)
	if isErr {
		t.Fatalf("valid edit failed: %s", out)
	}
	after, _ := os.ReadFile(f)
	if !strings.Contains(string(after), "x := 42") {
		t.Errorf("edit not applied:\n%s", after)
	}
}

// Lint gate: an edit that breaks syntax is rejected and rolled back.
func TestExecuteEditV2_LintGateRollsBack(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"
	os.WriteFile(f, []byte(original), 0644)

	// Remove the closing brace → broken Go.
	out, isErr := executeEditV2WithOptions(
		editInput(t, "main.go", "\tprintln(\"hi\")\n}", "\tprintln(\"hi\")"),
		dir,
		secureGoLintFixtureOptions(t),
	)
	if !isErr {
		t.Fatalf("expected lint gate to reject broken edit, got success: %s", out)
	}
	after, _ := os.ReadFile(f)
	if string(after) != original {
		t.Errorf("file was NOT rolled back to original:\n%s", after)
	}
}

// Fuzzy: old_string with mismatched indentation still matches and applies.
func TestExecuteEditV2_FuzzyMatch(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "main.go")
	// File uses tab indentation.
	os.WriteFile(f, []byte("package main\n\nfunc main() {\n\tx := 1\n\t_ = x\n}\n"), 0644)

	// old_string quotes it with 4 spaces — exact match fails, fuzzy should win.
	out, isErr := executeEditV2WithOptions(
		editInput(t, "main.go", "    x := 1", "\tx := 2"),
		dir,
		secureGoLintFixtureOptions(t),
	)
	if isErr {
		t.Fatalf("fuzzy match should have applied the edit, got error: %s", out)
	}
	after, _ := os.ReadFile(f)
	if !strings.Contains(string(after), "x := 2") {
		t.Errorf("fuzzy edit not applied:\n%s", after)
	}
}

// No match at all: returns an error with a hint, file untouched.
func TestExecuteEditV2_NoMatchHint(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "main.go")
	original := "package main\n\nfunc main() {\n\tx := 1\n}\n"
	os.WriteFile(f, []byte(original), 0644)

	out, isErr := executeEditV2WithOptions(
		editInput(t, "main.go", "this text does not exist", "y"),
		dir,
		secureGoLintFixtureOptions(t),
	)
	if !isErr {
		t.Fatalf("expected error for missing text, got success: %s", out)
	}
	after, _ := os.ReadFile(f)
	if string(after) != original {
		t.Errorf("file should be untouched on no-match:\n%s", after)
	}
}
