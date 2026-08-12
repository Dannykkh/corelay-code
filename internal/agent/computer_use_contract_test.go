package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeHostInteractionDriver struct {
	mu       sync.Mutex
	name     string
	caps     HostInteractionCapabilities
	calls    int
	requests []HostInteractionRequest
	execute  func(context.Context, HostInteractionRequest) (HostInteractionResponse, error)
}

var testHostApprovalSequence atomic.Uint64

func (d *fakeHostInteractionDriver) Name() string { return d.name }

func (d *fakeHostInteractionDriver) Capabilities() HostInteractionCapabilities { return d.caps }

func (d *fakeHostInteractionDriver) Execute(
	ctx context.Context,
	request HostInteractionRequest,
) (HostInteractionResponse, error) {
	d.mu.Lock()
	d.calls++
	d.requests = append(d.requests, request)
	d.mu.Unlock()
	if d.execute != nil {
		return d.execute(ctx, request)
	}
	return HostInteractionResponse{}, nil
}

func (d *fakeHostInteractionDriver) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func allHostCapabilities() HostInteractionCapabilities {
	return HostInteractionCapabilities{
		Platform:       runtime.GOOS,
		WindowControl:  true,
		InputControl:   true,
		Screenshot:     true,
		Clipboard:      true,
		WorkspaceFiles: true,
	}
}

func approvedHostOptions(
	t *testing.T,
	name string,
	input json.RawMessage,
	driver HostInteractionDriver,
) HostInteractionExecutionOptions {
	t.Helper()
	proof, err := mintHostInteractionApproval(
		fmt.Sprintf("approval-test-%s-%d", t.Name(), testHostApprovalSequence.Add(1)),
		"session-test",
		"run-test",
		name,
		input,
		time.Now().Add(time.Minute),
	)
	if err != nil {
		t.Fatalf("mint host approval: %v", err)
	}
	return HostInteractionExecutionOptions{
		Context:           context.Background(),
		Driver:            driver,
		ExpectedSessionID: "session-test",
		ExpectedRunID:     "run-test",
		Policy: HostInteractionPolicy{
			Enabled:     true,
			Allowed:     allHostCapabilities(),
			CallTimeout: time.Second,
		},
		approval: proof,
	}
}

func TestComputerUseZeroAndMissingApprovalFailClosedWithoutDriverStart(t *testing.T) {
	input := json.RawMessage(`{"region":"full"}`)
	driver := &fakeHostInteractionDriver{name: "fake-host", caps: allHostCapabilities()}

	result, isError, handled := ExecuteComputerUseTool("Screenshot", input, t.TempDir())
	if !handled || !isError || !strings.Contains(result, string(HostFailureConfigurationInvalid)) {
		t.Fatalf("zero result=%q isError=%v handled=%v", result, isError, handled)
	}

	opts := approvedHostOptions(t, "Screenshot", input, driver)
	opts.approval = hostInteractionApprovalProof{}
	result, isError, handled = ExecuteComputerUseToolWithOptions("Screenshot", input, t.TempDir(), opts)
	if !handled || !isError || !strings.Contains(result, string(HostFailureApprovalRequired)) {
		t.Fatalf("missing approval result=%q isError=%v handled=%v", result, isError, handled)
	}
	if driver.callCount() != 0 {
		t.Fatalf("driver calls=%d, want 0", driver.callCount())
	}
}

func TestComputerUseCapabilityAndPlatformMismatchFailBeforeStart(t *testing.T) {
	input := json.RawMessage(`{"region":"active"}`)
	driver := &fakeHostInteractionDriver{
		name: "fake-host",
		caps: HostInteractionCapabilities{
			Platform:      runtime.GOOS,
			WindowControl: true,
		},
	}
	opts := approvedHostOptions(t, "Screenshot", input, driver)

	result, isError, _ := ExecuteComputerUseToolWithOptions("Screenshot", input, t.TempDir(), opts)
	if !isError || !strings.Contains(result, string(HostFailureCapabilityUnavailable)) {
		t.Fatalf("capability result=%q isError=%v", result, isError)
	}
	if driver.callCount() != 0 {
		t.Fatalf("capability mismatch started driver %d times", driver.callCount())
	}

	driver.caps = allHostCapabilities()
	driver.caps.Platform = "unsupported-os"
	var report HostInteractionReport
	opts.ObserveReport = func(value HostInteractionReport) { report = value }
	result, isError, _ = ExecuteComputerUseToolWithOptions("Screenshot", input, t.TempDir(), opts)
	if !isError || !strings.Contains(result, string(HostFailureUnsupportedPlatform)) {
		t.Fatalf("platform result=%q isError=%v", result, isError)
	}
	if driver.callCount() != 0 {
		t.Fatalf("unsupported platform started driver %d times", driver.callCount())
	}
	if report.Failure != HostFailureUnsupportedPlatform || report.Started {
		t.Fatalf("unsupported platform report=%#v", report)
	}
}

func TestComputerUseCancellationAndTimeoutPropagate(t *testing.T) {
	input := json.RawMessage(`{"text":"not-reported"}`)
	driver := &fakeHostInteractionDriver{
		name: "fake-host",
		caps: allHostCapabilities(),
		execute: func(ctx context.Context, _ HostInteractionRequest) (HostInteractionResponse, error) {
			<-ctx.Done()
			return HostInteractionResponse{}, ctx.Err()
		},
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	opts := approvedHostOptions(t, "TypeText", input, driver)
	opts.Context = canceled
	result, isError, _ := ExecuteComputerUseToolWithOptions("TypeText", input, t.TempDir(), opts)
	if !isError || !strings.Contains(result, string(HostFailureCanceled)) {
		t.Fatalf("cancel result=%q isError=%v", result, isError)
	}
	if driver.callCount() != 0 {
		t.Fatalf("pre-canceled context started driver %d times", driver.callCount())
	}

	opts = approvedHostOptions(t, "TypeText", input, driver)
	opts.Policy.CallTimeout = 10 * time.Millisecond
	result, isError, _ = ExecuteComputerUseToolWithOptions("TypeText", input, t.TempDir(), opts)
	if !isError || !strings.Contains(result, string(HostFailureTimedOut)) {
		t.Fatalf("timeout result=%q isError=%v", result, isError)
	}
	if driver.callCount() != 1 {
		t.Fatalf("timeout driver calls=%d, want 1", driver.callCount())
	}
}

func TestComputerUseReportAndResultRedactRawInputAndOutput(t *testing.T) {
	secret := "operator-secret-DO-NOT-REPORT"
	input, _ := json.Marshal(map[string]string{"text": secret})
	driver := &fakeHostInteractionDriver{
		name: "fake-host",
		caps: allHostCapabilities(),
		execute: func(_ context.Context, request HostInteractionRequest) (HostInteractionResponse, error) {
			if request.Text != secret {
				t.Fatalf("driver text=%q", request.Text)
			}
			return HostInteractionResponse{Payload: []byte(secret)}, nil
		},
	}
	opts := approvedHostOptions(t, "TypeText", input, driver)
	var observed HostInteractionReport
	opts.ObserveReport = func(report HostInteractionReport) { observed = report }

	result, isError, _ := ExecuteComputerUseToolWithOptions("TypeText", input, t.TempDir(), opts)
	if isError {
		t.Fatalf("result=%q", result)
	}
	encoded, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result, secret) || strings.Contains(string(encoded), secret) {
		t.Fatalf("secret leaked: result=%q report=%s", result, encoded)
	}
	if observed.Action != HostActionTypeText || observed.InputDigest == "" || observed.OutputDigest == "" || !observed.Started {
		t.Fatalf("report=%#v", observed)
	}
}

func TestComputerUseRedactsDriverErrorsAndBoundsClipboardOutput(t *testing.T) {
	secret := "driver-secret-DO-NOT-REPORT"
	input := json.RawMessage(`{"action":"read"}`)
	driver := &fakeHostInteractionDriver{
		name: "fake-host",
		caps: allHostCapabilities(),
		execute: func(context.Context, HostInteractionRequest) (HostInteractionResponse, error) {
			return HostInteractionResponse{}, &HostInteractionError{Detail: secret}
		},
	}
	opts := approvedHostOptions(t, "Clipboard", input, driver)
	var report HostInteractionReport
	opts.ObserveReport = func(value HostInteractionReport) { report = value }
	result, isError, _ := ExecuteComputerUseToolWithOptions("Clipboard", input, t.TempDir(), opts)
	encoded, _ := json.Marshal(report)
	if !isError || strings.Contains(result, secret) || strings.Contains(string(encoded), secret) {
		t.Fatalf("driver error leaked: result=%q report=%s", result, encoded)
	}

	driver.execute = func(context.Context, HostInteractionRequest) (HostInteractionResponse, error) {
		return HostInteractionResponse{Payload: make([]byte, 64*1024+1)}, nil
	}
	opts = approvedHostOptions(t, "Clipboard", input, driver)
	result, isError, _ = ExecuteComputerUseToolWithOptions("Clipboard", input, t.TempDir(), opts)
	if !isError || !strings.Contains(result, "safe bound") {
		t.Fatalf("oversized clipboard result=%q isError=%v", result, isError)
	}
}

func TestComputerUseScreenshotReportsOnlyDigestDimensionsAndSize(t *testing.T) {
	input := json.RawMessage(`{"region":"full"}`)
	driver := &fakeHostInteractionDriver{
		name: "fake-host",
		caps: allHostCapabilities(),
		execute: func(context.Context, HostInteractionRequest) (HostInteractionResponse, error) {
			return HostInteractionResponse{Payload: []byte("raw-image-bytes"), Width: 1280, Height: 720}, nil
		},
	}
	opts := approvedHostOptions(t, "Screenshot", input, driver)
	var report HostInteractionReport
	opts.ObserveReport = func(value HostInteractionReport) { report = value }

	result, isError, _ := ExecuteComputerUseToolWithOptions("Screenshot", input, t.TempDir(), opts)
	if isError || !strings.Contains(result, "dimensions=1280x720") || strings.Contains(result, "raw-image-bytes") {
		t.Fatalf("result=%q isError=%v", result, isError)
	}
	if report.Width != 1280 || report.Height != 720 || report.OutputBytes != len("raw-image-bytes") || report.OutputDigest == "" {
		t.Fatalf("report=%#v", report)
	}
}

func TestComputerUseApprovalProofIsBoundToExactInput(t *testing.T) {
	approvedInput := json.RawMessage(`{"text":"approved"}`)
	changedInput := json.RawMessage(`{"text":"changed"}`)
	driver := &fakeHostInteractionDriver{name: "fake-host", caps: allHostCapabilities()}
	opts := approvedHostOptions(t, "TypeText", approvedInput, driver)

	result, isError, _ := ExecuteComputerUseToolWithOptions("TypeText", changedInput, t.TempDir(), opts)
	if !isError || !strings.Contains(result, string(HostFailureApprovalInvalid)) {
		t.Fatalf("result=%q isError=%v", result, isError)
	}
	if driver.callCount() != 0 {
		t.Fatalf("changed input started driver %d times", driver.callCount())
	}
}

func TestComputerUseApprovalProofCannotReplayAcrossRunOrCall(t *testing.T) {
	input := json.RawMessage(`{"text":"approved"}`)
	driver := &fakeHostInteractionDriver{name: "fake-host", caps: allHostCapabilities()}
	opts := approvedHostOptions(t, "TypeText", input, driver)
	opts.ExpectedRunID = "different-run"

	result, isError, _ := ExecuteComputerUseToolWithOptions("TypeText", input, t.TempDir(), opts)
	if !isError || !strings.Contains(result, string(HostFailureApprovalInvalid)) || driver.callCount() != 0 {
		t.Fatalf("cross-run result=%q isError=%v calls=%d", result, isError, driver.callCount())
	}

	opts.ExpectedRunID = "run-test"
	result, isError, _ = ExecuteComputerUseToolWithOptions("TypeText", input, t.TempDir(), opts)
	if isError || driver.callCount() != 1 {
		t.Fatalf("first execution result=%q isError=%v calls=%d", result, isError, driver.callCount())
	}
	result, isError, _ = ExecuteComputerUseToolWithOptions("TypeText", input, t.TempDir(), opts)
	if !isError || !strings.Contains(result, string(HostFailureApprovalInvalid)) || driver.callCount() != 1 {
		t.Fatalf("replay result=%q isError=%v calls=%d", result, isError, driver.callCount())
	}
}

func TestComputerUseApprovalRegistrySweepsExpiredEntries(t *testing.T) {
	expiredKey := fmt.Sprintf("expired-%s-%d", t.Name(), testHostApprovalSequence.Add(1))
	usedHostInteractionApprovals.Store(expiredKey, time.Now().Add(-time.Second).UnixNano())
	t.Cleanup(func() { usedHostInteractionApprovals.Delete(expiredKey) })

	input := json.RawMessage(`{"text":"approved"}`)
	driver := &fakeHostInteractionDriver{name: "fake-host", caps: allHostCapabilities()}
	opts := approvedHostOptions(t, "TypeText", input, driver)
	if result, isError, _ := ExecuteComputerUseToolWithOptions("TypeText", input, t.TempDir(), opts); isError {
		t.Fatalf("execution result=%q", result)
	}
	if _, exists := usedHostInteractionApprovals.Load(expiredKey); exists {
		t.Fatal("expired approval registry entry was not swept")
	}
}

func TestDispatchComputerUseRequiresBrokerAndCarriesApprovalProof(t *testing.T) {
	input := json.RawMessage(`{"text":"approved host input"}`)
	workDir := t.TempDir()
	call := toolUseBlock{
		ID:       "host-call",
		Name:     "TypeText",
		Input:    input,
		InputRaw: string(input),
	}
	driver := &fakeHostInteractionDriver{name: "fake-host", caps: allHostCapabilities()}
	base := toolDispatchOptions{
		Context:          context.Background(),
		WorkDir:          workDir,
		AllowedTools:     toolCatalogNames(ComputerUseToolDefs()),
		PermissionConfig: PermissionConfig{AutoApprove: "all"},
		SessionID:        "host-session",
		RunID:            "host-run",
		Execute: func(call toolUseBlock) (string, bool) {
			return ExecuteToolWithOptions(call.Name, call.Input, workDir, ToolExecutionOptions{
				Context:           context.Background(),
				HostDriver:        driver,
				ExpectedSessionID: "host-session",
				ExpectedRunID:     "host-run",
				HostPolicy: HostInteractionPolicy{
					Enabled:     true,
					Allowed:     allHostCapabilities(),
					CallTimeout: time.Second,
				},
			})
		},
	}

	denied := dispatchToolCalls([]toolUseBlock{call}, base)
	if len(denied) != 1 || !denied[0].Synthetic || driver.callCount() != 0 {
		t.Fatalf("no-broker result=%#v driver calls=%d", denied, driver.callCount())
	}

	base.ApprovalRequester = &allowingApprovalRequester{}
	allowed := dispatchToolCalls([]toolUseBlock{call}, base)
	if len(allowed) != 1 || allowed[0].IsError || !allowed[0].Executed {
		t.Fatalf("approved result=%#v", allowed)
	}
	if driver.callCount() != 1 {
		t.Fatalf("approved driver calls=%d, want 1", driver.callCount())
	}
}

func TestComputerUseFileManagerCannotExpandHostFilesystemBoundary(t *testing.T) {
	workDir := t.TempDir()
	input, _ := json.Marshal(map[string]string{
		"action":      "copy",
		"source":      ".",
		"destination": "copy",
	})
	driver := &fakeHostInteractionDriver{name: "fake-host", caps: allHostCapabilities()}
	opts := approvedHostOptions(t, "FileManager", input, driver)

	result, isError, _ := ExecuteComputerUseToolWithOptions("FileManager", input, workDir, opts)
	if !isError || !strings.Contains(result, string(HostFailureCapabilityUnavailable)) {
		t.Fatalf("result=%q isError=%v", result, isError)
	}
	if driver.callCount() != 0 {
		t.Fatalf("FileManager started host driver %d times", driver.callCount())
	}
}

func TestComputerUseCatalogAdvertisesOnlyConfiguredCapabilities(t *testing.T) {
	hostNames := make(map[string]struct{})
	for _, definition := range ComputerUseToolDefs() {
		hostNames[definition.Name] = struct{}{}
	}
	for _, definition := range AllToolDefs(t.TempDir()) {
		if _, host := hostNames[definition.Name]; host {
			t.Fatalf("default catalog advertised unavailable host tool %q", definition.Name)
		}
	}

	driver := &fakeHostInteractionDriver{
		name: "clipboard-only",
		caps: HostInteractionCapabilities{
			Platform:  runtime.GOOS,
			Clipboard: true,
		},
	}
	policy := HostInteractionPolicy{
		Enabled: true,
		Allowed: HostInteractionCapabilities{
			Platform:  runtime.GOOS,
			Clipboard: true,
		},
		CallTimeout: time.Second,
	}
	available := availableComputerUseToolDefs(driver, policy)
	if len(available) != 1 || available[0].Name != "Clipboard" {
		t.Fatalf("available host tools=%#v", available)
	}

	driver.caps = allHostCapabilities()
	policy.Allowed = allHostCapabilities()
	available = availableComputerUseToolDefs(driver, policy)
	for _, definition := range available {
		if definition.Name == "FileManager" {
			t.Fatal("FileManager must never be advertised through host capabilities")
		}
	}
	if len(available) != len(ComputerUseToolDefs())-1 {
		t.Fatalf("full host catalog size=%d, want %d", len(available), len(ComputerUseToolDefs())-1)
	}
}
