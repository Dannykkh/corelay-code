package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func openSessionMemoryForTest(t *testing.T, baseDir, sessionID string) *SessionMemory {
	t.Helper()
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		t.Fatalf("create canonical test base: %v", err)
	}
	canonicalBase, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		t.Fatalf("canonicalize test base: %v", err)
	}
	store, err := OpenSessionMemory(canonicalBase, sessionID)
	if err != nil {
		t.Fatalf("OpenSessionMemory(%q): %v", sessionID, err)
	}
	return store
}

func TestOpenSessionMemoryAcceptsOrdinaryBasePath(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "ordinary-new-base")
	store, err := OpenSessionMemory(baseDir, "ordinary-session")
	if err != nil || store == nil {
		t.Fatalf("OpenSessionMemory(%q) = %#v, %v; want usable store", baseDir, store, err)
	}
}

func sessionMemoryResultID(content string) string {
	digest := sha256.Sum256([]byte(content))
	return "result_" + hex.EncodeToString(digest[:])
}

func sessionMemoryLargeResult(marker string) string {
	return strings.Repeat(marker, maxInlineToolResultBytes/len(marker)+2)
}

func TestSessionMemoryContentAddressedStoreAndLoad(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "memory")
	sessionID := "private-session-label"
	store := openSessionMemoryForTest(t, baseDir, sessionID)

	inline := strings.Repeat("i", maxInlineToolResultBytes)
	if got, replaced := store.StoreResult("read", inline); replaced || got != inline {
		t.Fatalf("inline result = (%d bytes, %v), want unchanged and not replaced", len(got), replaced)
	}

	content := sessionMemoryLargeResult("content-addressed/")
	reference, replaced := store.StoreResult("read", content)
	if !replaced {
		t.Fatal("large result was not replaced with a reference")
	}
	wantID := sessionMemoryResultID(content)
	if !strings.Contains(reference, wantID) {
		t.Fatalf("reference %q does not contain digest ID %q", reference, wantID)
	}
	if strings.Contains(reference, baseDir) || strings.Contains(reference, sessionID) {
		t.Fatalf("reference disclosed a storage path or session label: %q", reference)
	}

	secondReference, replaced := store.StoreResult("read", content)
	if !replaced || secondReference != reference {
		t.Fatalf("duplicate store = (%q, %v), want identical reference", secondReference, replaced)
	}

	digest := strings.TrimPrefix(wantID, "result_")
	for _, id := range []string{wantID, digest, wantID + ".blob"} {
		got, err := store.LoadResult(id)
		if err != nil {
			t.Errorf("LoadResult(%q): %v", id, err)
			continue
		}
		if got != content {
			t.Errorf("LoadResult(%q) returned %d bytes, want original %d bytes", id, len(got), len(content))
		}
	}

	entries, err := os.ReadDir(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != wantID+".blob" {
		t.Fatalf("stored entries = %#v, want one digest blob", entries)
	}
	data, err := os.ReadFile(filepath.Join(store.dir, entries[0].Name()))
	if err != nil || string(data) != content {
		t.Fatalf("persisted content mismatch: bytes=%d err=%v", len(data), err)
	}
}

func TestSessionMemorySessionLabelIsPrivateAndCannotTraverse(t *testing.T) {
	testRoot := t.TempDir()
	baseDir := filepath.Join(testRoot, "storage")
	sessionID := filepath.Join("..", "..", "outside", "sensitive-session")
	store := openSessionMemoryForTest(t, baseDir, sessionID)

	wantKey := sessionMemoryKey(sessionID)
	if filepath.Base(store.dir) != wantKey || filepath.Base(store.root) != "session-output" {
		t.Fatalf("store paths = root %q dir %q, want digest child %q", store.root, store.dir, wantKey)
	}
	relative, err := filepath.Rel(store.root, store.dir)
	if err != nil || relative != wantKey || filepath.Dir(relative) != "." {
		t.Fatalf("session directory escaped root: relative=%q err=%v", relative, err)
	}
	if strings.Contains(store.dir, "sensitive-session") || strings.Contains(store.Stats().SessionDir, "sensitive-session") {
		t.Fatalf("session label leaked through store metadata: dir=%q stats=%#v", store.dir, store.Stats())
	}
	if _, err := os.Stat(filepath.Join(testRoot, "outside")); !os.IsNotExist(err) {
		t.Fatalf("session traversal created an outside path: %v", err)
	}
	if _, err := store.LoadResult(filepath.Join("..", "outside")); err == nil {
		t.Fatal("LoadResult accepted a traversal ID")
	}

	invalidLabels := []string{
		"",
		"   ",
		"line\nbreak",
		string([]byte{0xff}),
		strings.Repeat("x", maxSessionMemoryIDBytes+1),
	}
	for _, label := range invalidLabels {
		if opened, err := OpenSessionMemory(filepath.Join(testRoot, "invalid"), label); err == nil || opened != nil {
			t.Errorf("OpenSessionMemory accepted invalid label %q", label)
		}
	}
}

func TestOpenSessionMemoryRejectsSymlinkedBaseWithoutOutsideWrites(t *testing.T) {
	testRoot := t.TempDir()
	outside := filepath.Join(testRoot, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(testRoot, "base-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}

	escapedChild := filepath.Join(outside, "must-not-be-created")
	store, err := OpenSessionMemory(filepath.Join(link, "must-not-be-created"), "session")
	if err == nil || store != nil {
		t.Fatalf("OpenSessionMemory through a symlink = %#v, %v; want rejection", store, err)
	}
	if _, statErr := os.Stat(escapedChild); !os.IsNotExist(statErr) {
		t.Fatalf("rejected symlinked base created outside state: %v", statErr)
	}

	existingOutsideChild := filepath.Join(outside, "already-existing")
	if err := os.Mkdir(existingOutsideChild, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err = OpenSessionMemory(filepath.Join(link, "already-existing"), "session")
	if err == nil || store != nil {
		t.Fatalf("OpenSessionMemory through a symlink with an existing descendant = %#v, %v; want rejection", store, err)
	}
	if _, statErr := os.Stat(filepath.Join(existingOutsideChild, "session-output")); !os.IsNotExist(statErr) {
		t.Fatalf("rejected existing symlink descendant created outside session state: %v", statErr)
	}
}

func TestSessionMemoryReferencePreviewPreservesUTF8(t *testing.T) {
	store := openSessionMemoryForTest(t, t.TempDir(), "utf8-preview")
	content := strings.Repeat("a", toolResultPreviewBytes-1) + "界" + strings.Repeat("tail", 500)
	reference, replaced := store.StoreResult("read", content)
	if !replaced {
		t.Fatal("large UTF-8 result was not stored")
	}
	lines := strings.Split(reference, "\n")
	if len(lines) != 3 {
		t.Fatalf("reference has %d lines, want 3: %q", len(lines), reference)
	}
	preview := lines[1]
	if !utf8.ValidString(preview) {
		t.Fatalf("preview is not valid UTF-8: %q", preview)
	}
	wantPreview := strings.Repeat("a", toolResultPreviewBytes-1) + "..."
	if preview != wantPreview {
		t.Fatalf("preview = %q, want %q", preview, wantPreview)
	}
}

func TestSessionMemoryConcurrentDedupeAcrossInstances(t *testing.T) {
	baseDir := t.TempDir()
	const instanceCount = 24
	stores := make([]*SessionMemory, instanceCount)
	for index := range stores {
		stores[index] = openSessionMemoryForTest(t, baseDir, "shared-session")
	}
	content := sessionMemoryLargeResult("concurrent-content/")
	type outcome struct {
		reference string
		replaced  bool
	}
	outcomes := make(chan outcome, instanceCount)
	start := make(chan struct{})
	var group sync.WaitGroup
	for _, store := range stores {
		group.Add(1)
		go func(store *SessionMemory) {
			defer group.Done()
			<-start
			reference, replaced := store.StoreResult("parallel", content)
			outcomes <- outcome{reference: reference, replaced: replaced}
		}(store)
	}
	close(start)
	group.Wait()
	close(outcomes)

	var wantReference string
	for result := range outcomes {
		if !result.replaced {
			t.Fatal("a concurrent store failed to return a reference")
		}
		if wantReference == "" {
			wantReference = result.reference
		} else if result.reference != wantReference {
			t.Fatalf("concurrent references differ: %q != %q", result.reference, wantReference)
		}
	}

	entries, err := os.ReadDir(stores[0].dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != sessionMemoryResultID(content)+".blob" {
		t.Fatalf("concurrent store entries = %#v, want one deduplicated blob", entries)
	}
	stats := stores[len(stores)-1].Stats()
	if stats.ResultCount != 1 || stats.TotalBytes != int64(len(content)) {
		t.Fatalf("deduplicated stats = %#v", stats)
	}
}

func TestSessionMemoryRejectsCorruptAndSymlinkBlobs(t *testing.T) {
	t.Run("corrupt blob", func(t *testing.T) {
		store := openSessionMemoryForTest(t, t.TempDir(), "corrupt")
		content := sessionMemoryLargeResult("original/")
		if _, replaced := store.StoreResult("read", content); !replaced {
			t.Fatal("failed to seed result blob")
		}
		blobPath := filepath.Join(store.dir, sessionMemoryResultID(content)+".blob")
		if err := os.WriteFile(blobPath, []byte(strings.Repeat("x", len(content))), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.LoadResult(sessionMemoryResultID(content)); err == nil {
			t.Fatal("LoadResult accepted a corrupt blob")
		}
		if results := store.ListResults(); len(results) != 0 {
			t.Fatalf("ListResults included a corrupt blob: %#v", results)
		}
		if total := store.TotalSize(); total != 0 {
			t.Fatalf("TotalSize with corruption = %d, want fail-closed zero", total)
		}
		if stats := store.Stats(); stats != (SessionMemoryStats{}) {
			t.Fatalf("Stats with corruption = %#v, want fail-closed zero value", stats)
		}
		other := sessionMemoryLargeResult("other/")
		if got, replaced := store.StoreResult("read", other); replaced || got != other {
			t.Fatal("StoreResult wrote alongside a corrupt blob")
		}
		if err := store.CleanupSafe(); err == nil {
			t.Fatal("CleanupSafe accepted a corrupt blob")
		}
	})

	t.Run("symlink blob", func(t *testing.T) {
		testRoot := t.TempDir()
		store := openSessionMemoryForTest(t, filepath.Join(testRoot, "store"), "symlink")
		content := sessionMemoryLargeResult("outside/")
		outside := filepath.Join(testRoot, "outside-canary.blob")
		if err := os.WriteFile(outside, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		blobPath := filepath.Join(store.dir, sessionMemoryResultID(content)+".blob")
		if err := os.Symlink(outside, blobPath); err != nil {
			t.Skipf("symlinks are unavailable: %v", err)
		}
		if _, err := store.LoadResult(sessionMemoryResultID(content)); err == nil {
			t.Fatal("LoadResult followed a blob symlink")
		}
		if results := store.ListResults(); len(results) != 0 {
			t.Fatalf("ListResults included a blob symlink: %#v", results)
		}
		if got, replaced := store.StoreResult("read", content); replaced || got != content {
			t.Fatal("StoreResult accepted a blob symlink as a deduplicated result")
		}
		if err := store.CleanupSafe(); err == nil {
			t.Fatal("CleanupSafe accepted a blob symlink")
		}
		data, err := os.ReadFile(outside)
		if err != nil || string(data) != content {
			t.Fatalf("outside symlink canary changed: bytes=%d err=%v", len(data), err)
		}
	})
}

func TestSessionMemoryCleanupSafePreservesCanariesAndOtherSessions(t *testing.T) {
	testRoot := t.TempDir()
	baseDir := filepath.Join(testRoot, "store")
	first := openSessionMemoryForTest(t, baseDir, "first")
	second := openSessionMemoryForTest(t, baseDir, "second")
	firstContent := sessionMemoryLargeResult("first/")
	secondContent := sessionMemoryLargeResult("second/")
	if _, replaced := first.StoreResult("read", firstContent); !replaced {
		t.Fatal("failed to seed first session")
	}
	if _, replaced := second.StoreResult("read", secondContent); !replaced {
		t.Fatal("failed to seed second session")
	}
	if err := os.WriteFile(filepath.Join(first.dir, ".tmp_abandoned"), []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideCanary := filepath.Join(testRoot, "outside-canary.txt")
	if err := os.WriteFile(outsideCanary, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := first.CleanupSafe(); err != nil {
		t.Fatalf("CleanupSafe: %v", err)
	}
	if _, err := os.Stat(first.dir); !os.IsNotExist(err) {
		t.Fatalf("cleaned session directory still exists: %v", err)
	}
	if got, err := second.LoadResult(sessionMemoryResultID(secondContent)); err != nil || got != secondContent {
		t.Fatalf("cleanup changed another session: bytes=%d err=%v", len(got), err)
	}
	if data, err := os.ReadFile(outsideCanary); err != nil || string(data) != "keep" {
		t.Fatalf("cleanup changed outside canary: %q, %v", data, err)
	}
}

func TestSessionMemoryCleanupSafePreflightsBeforeMutation(t *testing.T) {
	store := openSessionMemoryForTest(t, t.TempDir(), "cleanup-preflight")
	content := sessionMemoryLargeResult("preserve-on-rejection/")
	if _, replaced := store.StoreResult("read", content); !replaced {
		t.Fatal("failed to seed result")
	}
	blobPath := filepath.Join(store.dir, sessionMemoryResultID(content)+".blob")
	canaryPath := filepath.Join(store.dir, "z-unrecognized-canary.txt")
	if err := os.WriteFile(canaryPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.CleanupSafe(); err == nil {
		t.Fatal("CleanupSafe accepted an unrecognized entry")
	}
	if data, err := os.ReadFile(canaryPath); err != nil || string(data) != "keep" {
		t.Fatalf("rejected cleanup changed canary: %q, %v", data, err)
	}
	if data, err := os.ReadFile(blobPath); err != nil || string(data) != content {
		t.Fatalf("rejected cleanup partially removed a valid blob: bytes=%d err=%v", len(data), err)
	}
}

func TestSessionMemoryListAndStatsAreDeterministic(t *testing.T) {
	sessionID := "deterministic-private-session"
	store := openSessionMemoryForTest(t, t.TempDir(), sessionID)
	contents := []string{
		sessionMemoryLargeResult("charlie/"),
		sessionMemoryLargeResult("alpha/"),
		sessionMemoryLargeResult("bravo/"),
	}
	want := make([]map[string]interface{}, 0, len(contents))
	var wantTotal int64
	for _, content := range contents {
		if _, replaced := store.StoreResult("read", content); !replaced {
			t.Fatal("failed to store deterministic fixture")
		}
		want = append(want, map[string]interface{}{
			"id":   sessionMemoryResultID(content),
			"size": int64(len(content)),
		})
		wantTotal += int64(len(content))
	}
	sort.Slice(want, func(i, j int) bool {
		return want[i]["id"].(string) < want[j]["id"].(string)
	})
	if err := os.WriteFile(filepath.Join(store.dir, "ignored-metadata"), []byte("not a blob"), 0o600); err != nil {
		t.Fatal(err)
	}

	first := store.ListResults()
	second := store.ListResults()
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
		t.Fatalf("ListResults is not deterministic:\nfirst=%#v\nsecond=%#v\nwant=%#v", first, second, want)
	}
	wantStats := SessionMemoryStats{
		ResultCount: len(contents),
		TotalBytes:  wantTotal,
		SessionDir:  sessionMemoryKey(sessionID),
	}
	if firstStats, secondStats := store.Stats(), store.Stats(); firstStats != wantStats || secondStats != wantStats {
		t.Fatalf("Stats = %#v then %#v, want %#v", firstStats, secondStats, wantStats)
	}
	if total := store.TotalSize(); total != wantTotal {
		t.Fatalf("TotalSize = %d, want %d", total, wantTotal)
	}
}

func TestNewSessionMemoryFailsClosed(t *testing.T) {
	store := NewSessionMemory("", "legacy-constructor")
	if store == nil || store.initErr == nil {
		t.Fatalf("invalid legacy constructor = %#v, want non-nil fail-closed store", store)
	}
	inline := "small"
	if got, replaced := store.StoreResult("read", inline); replaced || got != inline {
		t.Fatal("fail-closed store changed an inline result")
	}
	large := sessionMemoryLargeResult("legacy-fail-closed/")
	if got, replaced := store.StoreResult("read", large); replaced || got != large {
		t.Fatal("fail-closed store claimed to persist a large result")
	}
	if _, err := store.LoadResult(strings.Repeat("0", sha256.Size*2)); err == nil {
		t.Fatal("fail-closed store loaded a result")
	}
	if results := store.ListResults(); results != nil {
		t.Fatalf("fail-closed ListResults = %#v, want nil", results)
	}
	if total := store.TotalSize(); total != 0 {
		t.Fatalf("fail-closed TotalSize = %d, want zero", total)
	}
	if stats := store.Stats(); stats != (SessionMemoryStats{}) {
		t.Fatalf("fail-closed Stats = %#v, want zero value", stats)
	}
	if err := store.CleanupSafe(); err == nil {
		t.Fatal("fail-closed CleanupSafe returned nil")
	}
	store.Cleanup()
}

func TestAllowedSessionMemoryRootAliasIsDarwinOnlyAndExact(t *testing.T) {
	tests := []struct {
		goos     string
		lexical  string
		resolved string
		want     bool
	}{
		{goos: "darwin", lexical: "/var", resolved: "/private/var", want: true},
		{goos: "darwin", lexical: "/tmp", resolved: "/private/tmp", want: true},
		{goos: "darwin", lexical: "/var/folders/user-link", resolved: "/outside", want: false},
		{goos: "darwin", lexical: "/var", resolved: "/outside", want: false},
		{goos: "linux", lexical: "/var", resolved: "/private/var", want: false},
		{goos: "windows", lexical: "/tmp", resolved: "/private/tmp", want: false},
	}
	for _, test := range tests {
		if got := allowedSessionMemoryRootAlias(test.goos, test.lexical, test.resolved); got != test.want {
			t.Errorf("allowedSessionMemoryRootAlias(%q, %q, %q) = %v, want %v", test.goos, test.lexical, test.resolved, got, test.want)
		}
	}
}
