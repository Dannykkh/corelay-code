package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"github.com/Dannykkh/corelay-code/internal/types"
)

const (
	hardCrashHelperModeEnv = "CORELAY_CAP033_HELPER_MODE"
	hardCrashStateRootEnv  = "CORELAY_CAP033_STATE_ROOT"
	hardCrashWorkspaceEnv  = "CORELAY_CAP033_WORKSPACE"
	hardCrashSessionIDEnv  = "CORELAY_CAP033_SESSION_ID"
	hardCrashRevisionEnv   = "CORELAY_CAP033_REVISION"
	hardCrashCounterEnv    = "CORELAY_CAP033_COUNTER"
	hardCrashExitCode      = 86
	hardCrashRawInput      = "echo hard-crash-side-effect-secret"
)

type hardCrashSideEffectRunner struct {
	counterPath string
}

func (*hardCrashSideEffectRunner) Name() string { return "cap033-hard-crash" }

func (*hardCrashSideEffectRunner) Capabilities() sandbox.Capabilities {
	return resumeSandboxCapabilities()
}

func (r *hardCrashSideEffectRunner) Run(
	_ context.Context,
	_ sandbox.Policy,
	_ sandbox.CommandSpec,
) (sandbox.Result, sandbox.Report) {
	if err := incrementHardCrashCounter(r.counterPath); err != nil {
		os.Exit(hardCrashExitCode + 1)
	}
	// This is the exact gap under test: the external effect is durable, but no
	// result event, terminal event, defer, or Finalize call can run afterward.
	os.Exit(hardCrashExitCode)
	return sandbox.Result{}, sandbox.Report{}
}

func TestCAP033HardCrashHelper(t *testing.T) {
	mode := os.Getenv(hardCrashHelperModeEnv)
	if mode == "" {
		return
	}
	root := os.Getenv(hardCrashStateRootEnv)
	workDir := os.Getenv(hardCrashWorkspaceEnv)
	sessionID := os.Getenv(hardCrashSessionIDEnv)
	revision, err := strconv.ParseUint(os.Getenv(hardCrashRevisionEnv), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	store := agent.NewSessionStore(root)
	request := []types.Message{{Role: "user", Content: hardCrashJSON("perform the guarded operation once")}}
	if mode == "blocked" {
		if _, err := prepareDurableAgentRun(store, sessionID, revision, workDir, request); !errors.Is(err, agent.ErrSessionReconcileRequired) {
			t.Fatalf("fresh-process resume = %v, want reconcile required", err)
		}
		return
	}
	if mode != "crash" {
		t.Fatalf("unknown helper mode %q", mode)
	}
	run, err := prepareDurableAgentRun(store, sessionID, revision, workDir, request)
	if err != nil {
		t.Fatal(err)
	}
	provider := &resumeContinuationProvider{steps: []resumeContinuationStep{
		{toolID: "call-read-before-effect", toolName: "Read", toolInput: `{"file_path":"seed.txt"}`},
		{toolID: "call-hard-crash-effect", toolName: "Bash", toolInput: `{"command":"` + hardCrashRawInput + `"}`},
	}}
	runResumeContinuationKernel(
		t,
		context.Background(),
		provider,
		request,
		workDir,
		run,
		&hardCrashSideEffectRunner{counterPath: os.Getenv(hardCrashCounterEnv)},
	)
	t.Fatal("hard-crash helper returned without os.Exit")
}

func TestDurableSessionPreExecutionJournalSurvivesHardProcessExit(t *testing.T) {
	isolateResumeContinuationEnvironment(t)
	stateRoot := t.TempDir()
	workDir := t.TempDir()
	counterPath := filepath.Join(t.TempDir(), "side-effect-count")
	if err := os.WriteFile(filepath.Join(workDir, "seed.txt"), []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := agent.NewSessionStore(stateRoot)
	session := agent.Session{
		Workspace: workDir,
		Messages:  []agent.SessionMessage{{Role: "user", Content: "perform the guarded operation once"}},
	}
	if err := store.SaveExpected(&session, 0); err != nil {
		t.Fatal(err)
	}

	crashOutput := runCAP033Helper(t, "crash", stateRoot, workDir, session.ID, session.Revision, counterPath, hardCrashExitCode)
	freshStore := agent.NewSessionStore(stateRoot)
	interrupted, err := freshStore.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if interrupted.Revision != 3 || interrupted.LastCommittedRevision != 1 ||
		!interrupted.ReconcileRequired || interrupted.LifecycleStatus != agent.SessionLifecycleInterrupted ||
		interrupted.Interruption == nil {
		t.Fatalf("hard-crash session = %#v", interrupted)
	}
	marker := interrupted.Interruption
	if marker.RunID == "" || marker.ToolName != "multiple_tools" || marker.ToolCallID != "multiple" ||
		marker.SideEffectState != agent.SessionSideEffectMayHaveApplied ||
		!strings.HasPrefix(marker.InputDigest, "sha256:") ||
		!strings.Contains(marker.Summary, "2 tool executions") {
		t.Fatalf("hard-crash aggregate marker = %#v", marker)
	}
	assertHardCrashCounter(t, counterPath, 1)
	encodedMarker, _ := json.Marshal(marker)
	for _, leak := range []string{hardCrashRawInput, stateRoot, workDir, counterPath} {
		if strings.Contains(string(encodedMarker), leak) || strings.Contains(string(crashOutput), leak) {
			t.Fatalf("hard-crash evidence leaked %q: marker=%s output=%s", leak, encodedMarker, crashOutput)
		}
	}
	if len(interrupted.Messages) != 1 || interrupted.LastRunTerminal != nil {
		t.Fatalf("hard crash committed partial transcript/terminal: %#v", interrupted)
	}

	// A genuinely fresh process must observe the persisted quarantine before it
	// can construct a provider or executor continuation.
	runCAP033Helper(t, "blocked", stateRoot, workDir, session.ID, interrupted.Revision, counterPath, 0)
	assertHardCrashCounter(t, counterPath, 1)

	reconciled, err := freshStore.MarkReconciled(session.ID, interrupted.Revision)
	if err != nil {
		t.Fatal(err)
	}
	request := []types.Message{{Role: "user", Content: hardCrashJSON("perform the guarded operation once")}}
	continuation, err := prepareDurableAgentRun(
		freshStore, session.ID, reconciled.Revision, workDir, request,
	)
	if err != nil {
		t.Fatal(err)
	}
	provider := &resumeContinuationProvider{steps: []resumeContinuationStep{{
		text: "Continuation completed after explicit reconciliation without replay.",
	}}}
	events := runResumeContinuationKernel(
		t, context.Background(), provider, request, workDir, continuation, &resumeSideEffectRunner{},
	)
	committed, err := continuation.Finalize(provider.Name(), "resume-model")
	if err != nil {
		t.Fatal(err)
	}
	if committed.Revision != 5 || committed.ReconcileRequired || committed.Interruption != nil ||
		committed.LastRunTerminal == nil {
		t.Fatalf("post-reconcile continuation = %#v", committed)
	}
	if terminal, ok := terminalMetadataFromEvents(events); !ok || terminal.BlocksSuccess() {
		t.Fatalf("continuation terminal = (%#v, %v)", terminal, ok)
	}
	assertHardCrashCounter(t, counterPath, 1)
}

func runCAP033Helper(
	t *testing.T,
	mode, stateRoot, workDir, sessionID string,
	revision uint64,
	counterPath string,
	wantExit int,
) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCAP033HardCrashHelper$")
	home := t.TempDir()
	command.Env = append(os.Environ(),
		hardCrashHelperModeEnv+"="+mode,
		hardCrashStateRootEnv+"="+stateRoot,
		hardCrashWorkspaceEnv+"="+workDir,
		hardCrashSessionIDEnv+"="+sessionID,
		hardCrashRevisionEnv+"="+strconv.FormatUint(revision, 10),
		hardCrashCounterEnv+"="+counterPath,
		"HOME="+home,
		"USERPROFILE="+home,
		"XDG_CONFIG_HOME="+home,
		"CORELAY_CONFIG_DIR="+home,
		"CORELAY_MEMORY=off",
		"CORELAY_AUTOSKILL=off",
		"CORELAY_AUTOVERIFY=off",
	)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("helper %s timed out: %v output=%s", mode, ctx.Err(), output)
	}
	if wantExit == 0 {
		if err != nil {
			t.Fatalf("helper %s = %v output=%s", mode, err, output)
		}
		return output
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != wantExit {
		t.Fatalf("helper %s exit = %v, want %d output=%s", mode, err, wantExit, output)
	}
	return output
}

func incrementHardCrashCounter(path string) error {
	count := 0
	if data, err := os.ReadFile(path); err == nil {
		count, err = strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(file, "%d", count+1); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func assertHardCrashCounter(t *testing.T, path string, want int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || got != want {
		t.Fatalf("side-effect counter = %q (%v), want %d", data, err, want)
	}
}

func hardCrashJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
