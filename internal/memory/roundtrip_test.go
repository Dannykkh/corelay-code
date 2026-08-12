package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteThenReadEntry(t *testing.T) {
	workDir := t.TempDir()

	e := Entry{
		Name:        "Tests Must Hit Real DB",
		Description: "Integration tests must use a real DB instance, not mocks",
		Type:        TypeFeedback,
		Body: "Integration tests must hit a real database, not mocks.\n\n" +
			"**Why:** Prior incident where mock/prod divergence masked a broken migration.\n\n" +
			"**How to apply:** Apply to any test file under tests/integration/**.",
	}

	path, err := WriteEntry(workDir, "Tests Must Hit Real DB", e)
	if err != nil {
		t.Fatalf("WriteEntry: %v", err)
	}
	if filepath.Base(path) != "feedback_tests-must-hit-real-db.md" {
		t.Fatalf("unexpected filename: %s", filepath.Base(path))
	}

	got, err := ReadEntry(path)
	if err != nil {
		t.Fatalf("ReadEntry: %v", err)
	}

	if got.Name != e.Name {
		t.Errorf("Name: got %q, want %q", got.Name, e.Name)
	}
	if got.Description != e.Description {
		t.Errorf("Description: got %q, want %q", got.Description, e.Description)
	}
	if got.Type != e.Type {
		t.Errorf("Type: got %q, want %q", got.Type, e.Type)
	}
	if strings.TrimSpace(got.Body) != strings.TrimSpace(e.Body) {
		t.Errorf("Body mismatch:\nGOT:\n%s\nWANT:\n%s", got.Body, e.Body)
	}
	if got.File != path {
		t.Errorf("File: got %q, want %q", got.File, path)
	}
	if got.ModifiedUnix == 0 {
		t.Error("ModifiedUnix should be populated from stat")
	}
}

func TestScanAndFormatManifest(t *testing.T) {
	workDir := t.TempDir()

	// Two Entry files (frontmatter-bearing).
	_, err := WriteEntry(workDir, "role", Entry{
		Name: "User role", Description: "Senior Go engineer", Type: TypeUser, Body: "Works on proxy-go.",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = WriteEntry(workDir, "db", Entry{
		Name: "DB rule", Description: "No mocks in integration tests", Type: TypeFeedback, Body: "Body here.",
	})
	if err != nil {
		t.Fatal(err)
	}

	// One scaffold file — no frontmatter. Scan should skip it but
	// ScanRaw must still list it.
	scaffold := filepath.Join(MemoryDir(workDir), "architecture.md")
	if err := os.WriteFile(scaffold, []byte("# architecture\n\nhand-written notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := Scan(workDir)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 parseable entries, got %d", len(entries))
	}
	// Sorted by type (feedback < user).
	if entries[0].Type != TypeFeedback || entries[1].Type != TypeUser {
		t.Errorf("unexpected sort: %v %v", entries[0].Type, entries[1].Type)
	}

	raws, err := ScanRaw(workDir)
	if err != nil {
		t.Fatalf("ScanRaw: %v", err)
	}
	if len(raws) != 3 {
		t.Fatalf("expected 3 raw files, got %d (%v)", len(raws), raws)
	}

	manifest := FormatManifest(raws)
	if !strings.Contains(manifest, "architecture.md") ||
		!strings.Contains(manifest, "feedback_db.md") ||
		!strings.Contains(manifest, "user_role.md") {
		t.Errorf("manifest missing expected entries:\n%s", manifest)
	}
}

func TestScan_MissingDirIsEmpty(t *testing.T) {
	workDir := t.TempDir() // valid dir, but no memory subdir yet
	entries, err := Scan(workDir)
	if err != nil {
		t.Fatalf("Scan on missing memory dir should not error, got %v", err)
	}
	if entries != nil {
		t.Fatalf("expected nil entries on missing dir, got %v", entries)
	}
}

func TestUpdateIndex_PreservesHandEditedSections(t *testing.T) {
	workDir := t.TempDir()

	// User-authored MEMORY.md with a top section and markers already in place.
	initial := "# MEMORY.md\n\n" +
		"## Project Goals\n- Ship Corelay Code v1.1\n- Long-term memory working\n\n" +
		autoStartMarker + "\n" +
		"_No extracted memories yet._\n" +
		autoEndMarker + "\n"
	if err := os.WriteFile(Entrypoint(workDir), []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	entries := []Entry{
		{Name: "User role", Description: "Senior Go engineer", Type: TypeUser},
		{Name: "DB rule", Description: "No mocks in integration tests", Type: TypeFeedback},
	}

	trunc, err := UpdateIndex(workDir, entries)
	if err != nil {
		t.Fatalf("UpdateIndex: %v", err)
	}

	// Re-read the file and verify both the hand-written section and the
	// managed block are present.
	data, err := os.ReadFile(Entrypoint(workDir))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	if !strings.Contains(content, "## Project Goals") {
		t.Error("hand-edited Project Goals section was lost")
	}
	if !strings.Contains(content, "Ship Corelay Code v1.1") {
		t.Error("hand-edited bullet was lost")
	}
	if !strings.Contains(content, "### User") {
		t.Error("managed block missing ### User section")
	}
	if !strings.Contains(content, "User role") {
		t.Error("managed block missing User entry")
	}
	if !strings.Contains(content, "### Feedback") {
		t.Error("managed block missing ### Feedback section")
	}
	if !strings.Contains(content, "DB rule") {
		t.Error("managed block missing Feedback entry")
	}
	if !strings.Contains(content, "_No extracted memories yet._") == false {
		// The stale "no entries" placeholder should have been replaced —
		// if it is still present we did not actually rewrite the block.
		t.Error("managed block still shows the empty placeholder after UpdateIndex")
	}

	if trunc.LineCapped || trunc.ByteCapped {
		t.Error("small test content should not have triggered any cap")
	}
}

func TestUpdateIndex_CreatesScaffoldWhenMissing(t *testing.T) {
	workDir := t.TempDir()

	trunc, err := UpdateIndex(workDir, []Entry{
		{Name: "Solo entry", Description: "Just one", Type: TypeProject},
	})
	if err != nil {
		t.Fatalf("UpdateIndex: %v", err)
	}
	if trunc.LineCapped || trunc.ByteCapped {
		t.Error("scaffold should not be capped")
	}

	data, err := os.ReadFile(Entrypoint(workDir))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "# MEMORY.md") {
		t.Error("scaffold header missing")
	}
	if !strings.Contains(content, autoStartMarker) || !strings.Contains(content, autoEndMarker) {
		t.Error("scaffold missing markers")
	}
	if !strings.Contains(content, "Solo entry") {
		t.Error("scaffold missing the entry we wrote")
	}
}

func TestReadIndex_MissingReturnsPlaceholder(t *testing.T) {
	workDir := t.TempDir()
	trunc, err := ReadIndex(workDir)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if !strings.Contains(trunc.Content, "no index yet") {
		t.Errorf("expected placeholder, got %q", trunc.Content)
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	in := "multi\nline\n\ntext   with    spaces"
	got := oneLine(in)
	want := "multi line text with spaces"
	if got != want {
		t.Errorf("oneLine:\n got %q\nwant %q", got, want)
	}
}
