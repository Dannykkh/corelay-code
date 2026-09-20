package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func withConfigTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", dir)
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	return dir
}

func TestLoadCheckedDistinguishesMissingAndMalformed(t *testing.T) {
	dir := withConfigTestDir(t)
	cfg, exists, err := LoadChecked()
	if err != nil || exists || cfg.Port != DefaultConfig().Port {
		t.Fatalf("missing config = (%+v, %v, %v), want default, false, nil", cfg, exists, err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, malformed := range [][]byte{[]byte("{"), []byte("null"), []byte("[]")} {
		if err := os.WriteFile(ConfigPath(), malformed, 0600); err != nil {
			t.Fatal(err)
		}
		if _, exists, err := LoadChecked(); err == nil || !exists {
			t.Fatalf("malformed config %q exists=%v err=%v, want exists and parse error", malformed, exists, err)
		}
	}
}

func TestUpdateDoesNotOverwriteMalformedConfig(t *testing.T) {
	withConfigTestDir(t)
	original := []byte("{broken")
	if err := os.WriteFile(ConfigPath(), original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(func(cfg *Config) error {
		cfg.Port = 5555
		return nil
	}); err == nil {
		t.Fatal("Update succeeded with malformed existing config")
	}
	got, err := os.ReadFile(ConfigPath())
	if err != nil || string(got) != string(original) {
		t.Fatalf("config after failed update = %q, err=%v; want original bytes", got, err)
	}
}

func TestUpdateCallbackFailureLeavesConfigUnchanged(t *testing.T) {
	withConfigTestDir(t)
	initial := Config{Port: 4321, AccessToken: "unchanged"}
	if err := Save(initial); err != nil {
		t.Fatal(err)
	}
	_, err := Update(func(cfg *Config) error {
		cfg.AccessToken = "not-published"
		return errors.New("cancel update")
	})
	if err == nil {
		t.Fatal("Update returned nil for callback failure")
	}
	got, exists, err := LoadChecked()
	if err != nil || !exists || got.Port != initial.Port || got.AccessToken != initial.AccessToken {
		t.Fatalf("config after callback failure = (%+v, %v, %v)", got, exists, err)
	}
}

func TestUpdatePublishesPrivateConfigFile(t *testing.T) {
	withConfigTestDir(t)
	if _, err := Update(func(cfg *Config) error {
		cfg.AccessToken = "fixture-secret"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '/' && info.Mode().Perm() != 0600 {
		t.Fatalf("config file permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestConfigUpdateCrossProcessHelper(t *testing.T) {
	if os.Getenv("CORELAY_CONFIG_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	t.Setenv("CORELAY_CONFIG_DIR", os.Getenv("CORELAY_CONFIG_HELPER_DIR"))
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	value := os.Getenv("CORELAY_CONFIG_HELPER_VALUE")
	if _, err := Update(func(cfg *Config) error {
		cfg.Projects = append(cfg.Projects, Project{Path: value, Name: filepath.Base(value)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateSerializesIndependentProcessChanges(t *testing.T) {
	withConfigTestDir(t)
	const writers = 10
	type runningWriter struct {
		cmd    *exec.Cmd
		output bytes.Buffer
	}
	cmds := make([]*runningWriter, 0, writers)
	for i := 0; i < writers; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestConfigUpdateCrossProcessHelper$")
		writer := &runningWriter{cmd: cmd}
		cmd.Stdout = &writer.output
		cmd.Stderr = &writer.output
		cmd.Env = append(os.Environ(),
			"CORELAY_CONFIG_HELPER=1",
			"CORELAY_CONFIG_HELPER_DIR="+configDir(),
			fmt.Sprintf("CORELAY_CONFIG_HELPER_VALUE=writer-%02d", i),
		)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, writer)
	}
	for _, writer := range cmds {
		err := writer.cmd.Wait()
		if err != nil {
			t.Fatalf("config writer failed: %v: %s", err, writer.output.String())
		}
	}
	cfg, exists, err := LoadChecked()
	if err != nil || !exists {
		t.Fatalf("LoadChecked() = (%v, %v, %v)", exists, err, err)
	}
	got := make(map[string]bool, len(cfg.Projects))
	for _, project := range cfg.Projects {
		got[project.Path] = true
	}
	if len(got) != writers {
		t.Fatalf("persisted %d distinct updates, want %d (%s)", len(got), writers, strings.Join(sortedProjectPaths(cfg.Projects), ", "))
	}
	for i := 0; i < writers; i++ {
		want := fmt.Sprintf("writer-%02d", i)
		if !got[want] {
			t.Errorf("missing independent config update %q", want)
		}
	}
}

func sortedProjectPaths(projects []Project) []string {
	out := make([]string, len(projects))
	for i, project := range projects {
		out[i] = project.Path
	}
	return out
}
