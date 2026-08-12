package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	helperEnabled     = "CORELAY_SANDBOX_HELPER"
	helperMode        = "CORELAY_SANDBOX_HELPER_MODE"
	helperStdoutBytes = "CORELAY_SANDBOX_HELPER_STDOUT_BYTES"
	helperStderrBytes = "CORELAY_SANDBOX_HELPER_STDERR_BYTES"
	helperInterleaved = "CORELAY_SANDBOX_HELPER_INTERLEAVED"
	helperDelayMillis = "CORELAY_SANDBOX_HELPER_DELAY_MILLIS"
	helperReadyPath   = "CORELAY_SANDBOX_HELPER_READY"
	helperStartPath   = "CORELAY_SANDBOX_HELPER_START"
)

func TestValidatePolicyCapabilities(t *testing.T) {
	secure := Capabilities{
		FilesystemIsolation:  true,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
	if err := ValidatePolicy(Policy{
		Enforcement: EnforcementRequired,
		Required: Capabilities{
			FilesystemIsolation:  true,
			EnvironmentFiltering: true,
		},
	}, secure); err != nil {
		t.Fatalf("capable runner rejected: %v", err)
	}

	tests := []struct {
		name        string
		policy      Policy
		available   Capabilities
		failureCode FailureCode
	}{
		{
			name:        "required without isolation",
			policy:      Policy{Enforcement: EnforcementRequired},
			available:   Capabilities{EnvironmentFiltering: true, Timeouts: true},
			failureCode: FailureCapabilityUnavailable,
		},
		{
			name:        "preferred without isolation",
			policy:      Policy{Enforcement: EnforcementPreferred},
			available:   Capabilities{EnvironmentFiltering: true, Timeouts: true},
			failureCode: FailureCapabilityUnavailable,
		},
		{
			name: "missing requested network isolation",
			policy: Policy{
				Enforcement: EnforcementRequired,
				Required:    Capabilities{NetworkIsolation: true},
			},
			available:   secure,
			failureCode: FailureCapabilityUnavailable,
		},
		{
			name: "disabled contradicts isolation requirement",
			policy: Policy{
				Enforcement: EnforcementDisabled,
				Required:    Capabilities{FilesystemIsolation: true},
			},
			available:   secure,
			failureCode: FailurePolicyInvalid,
		},
		{
			name:        "zero enforcement is invalid",
			policy:      Policy{},
			available:   secure,
			failureCode: FailurePolicyInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePolicy(test.policy, test.available)
			var policyErr *PolicyError
			if !errors.As(err, &policyErr) {
				t.Fatalf("error = %T %v, want PolicyError", err, err)
			}
			if policyErr.Code != test.failureCode {
				t.Fatalf("failure code = %q, want %q", policyErr.Code, test.failureCode)
			}
		})
	}
}

func TestUnconfinedAndUnavailableRejectRequiredAndPreferred(t *testing.T) {
	runners := []Runner{
		NewUnconfinedRunner(),
		NewUnavailableRunner("fixture has no adapter"),
	}
	for _, runner := range runners {
		for _, enforcement := range []Enforcement{EnforcementRequired, EnforcementPreferred} {
			t.Run(runner.Name()+"_"+string(enforcement), func(t *testing.T) {
				result, report := runner.Run(context.Background(), Policy{Enforcement: enforcement}, helperCommand("success"))
				if result.Started || report.Started {
					t.Fatalf("process started under %s: result=%#v report=%#v", enforcement, result, report)
				}
				wantFailure := FailureCapabilityUnavailable
				if runner.Name() == "unavailable" {
					wantFailure = FailureRunnerUnavailable
				}
				if report.Failure != wantFailure {
					t.Fatalf("failure = %q, want %q", report.Failure, wantFailure)
				}
			})
		}
	}
}

func TestUnconfinedRunnerRunsOnlyExplicitDisabled(t *testing.T) {
	runner := NewUnconfinedRunner()
	result, report := runner.Run(context.Background(), Policy{
		Enforcement: EnforcementDisabled,
		Required: Capabilities{
			EnvironmentFiltering: true,
			Timeouts:             true,
		},
	}, helperCommand("success"))
	if result.Err != nil || !result.Started || result.ExitCode != 0 {
		t.Fatalf("result = %#v", result)
	}
	if report.Failure != FailureNone || report.EffectiveEnforcement != EnforcementDisabled {
		t.Fatalf("report = %#v", report)
	}
	if report.AppliedIsolation.HasIsolation() || !report.AppliedIsolation.IsZero() {
		t.Fatalf("unconfined runner claimed isolation: %#v", report.AppliedIsolation)
	}
	if runner.Capabilities().HasIsolation() {
		t.Fatalf("unconfined capabilities claim isolation: %#v", runner.Capabilities())
	}
}

func TestUnavailableRunnerSetupFailureIsNotStarted(t *testing.T) {
	runner := NewUnavailableRunner("adapter setup failed")
	result, report := runner.Run(context.Background(), Policy{Enforcement: EnforcementDisabled}, helperCommand("success"))
	if result.Started || report.Started || result.ExitCode != ExitNotStarted {
		t.Fatalf("unavailable runner reported start: result=%#v report=%#v", result, report)
	}
	if report.Failure != FailureRunnerUnavailable || !strings.Contains(report.Detail, "setup failed") {
		t.Fatalf("report = %#v", report)
	}
}

func TestUnconfinedRunnerPreservesOutputAndExitCode(t *testing.T) {
	result, report := NewUnconfinedRunner().Run(
		context.Background(),
		Policy{Enforcement: EnforcementDisabled},
		helperCommand("exit"),
	)
	if !result.Started || result.ExitCode != 7 {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(string(result.Stdout), "stdout-payload") {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	if !strings.Contains(string(result.Stderr), "stderr-payload") {
		t.Fatalf("stderr = %q", result.Stderr)
	}
	if report.Failure != FailureExecutionFailed {
		t.Fatalf("report = %#v", report)
	}
}

func TestUnconfinedRunnerTimeout(t *testing.T) {
	command := helperCommand("timeout")
	command.Timeout = time.Second
	result, report := NewUnconfinedRunner().Run(
		context.Background(),
		Policy{Enforcement: EnforcementDisabled},
		command,
	)
	if !result.Started || !result.TimedOut || report.Failure != FailureTimedOut {
		t.Fatalf("timeout result=%#v report=%#v", result, report)
	}
	if !strings.Contains(string(result.Stdout), "before-timeout") || !strings.Contains(string(result.Stderr), "timeout-stderr") {
		t.Fatalf("partial output was lost: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
}

func TestUnconfinedRunnerEnvironmentFiltering(t *testing.T) {
	t.Setenv("SANDBOX_ALLOWED", "inherited-value")
	t.Setenv("SANDBOX_SECRET", "must-not-cross-boundary")
	command := helperCommand("environment")
	command.Environment.Inherit = append(command.Environment.Inherit, "SANDBOX_ALLOWED")
	command.Environment.Set["SANDBOX_EXPLICIT"] = "explicit-value"

	result, report := NewUnconfinedRunner().Run(
		context.Background(),
		Policy{Enforcement: EnforcementDisabled, Required: Capabilities{EnvironmentFiltering: true}},
		command,
	)
	if result.Err != nil || result.ExitCode != 0 || report.Failure != FailureNone {
		t.Fatalf("result=%#v report=%#v stdout=%q stderr=%q", result, report, result.Stdout, result.Stderr)
	}
	if !strings.Contains(string(result.Stdout), "environment-filtered") {
		t.Fatalf("helper did not confirm filtered environment: %q", result.Stdout)
	}
}

func TestUnconfinedRunnerStartFailureIsNotStarted(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-executable")
	result, report := NewUnconfinedRunner().Run(
		context.Background(),
		Policy{Enforcement: EnforcementDisabled},
		CommandSpec{Path: missing},
	)
	if result.Started || report.Started || result.ExitCode != ExitNotStarted {
		t.Fatalf("start failure reported as started: result=%#v report=%#v", result, report)
	}
	if report.Failure != FailureStartFailed {
		t.Fatalf("report = %#v", report)
	}
}

func TestBuildEnvironmentIsDenyByDefault(t *testing.T) {
	t.Setenv("SANDBOX_NOT_INHERITED", "secret")
	environment, err := BuildEnvironment(EnvironmentSpec{})
	if err != nil {
		t.Fatalf("BuildEnvironment: %v", err)
	}
	if environment == nil || len(environment) != 0 {
		t.Fatalf("zero spec environment = %#v, want non-nil empty", environment)
	}
}

// TestSandboxHelperProcess is executed in a child copy of the test binary.
func TestSandboxHelperProcess(t *testing.T) {
	if os.Getenv(helperEnabled) != "1" {
		return
	}
	switch os.Getenv(helperMode) {
	case "success":
		fmt.Fprint(os.Stdout, "success")
	case "exit":
		fmt.Fprint(os.Stdout, "stdout-payload")
		fmt.Fprint(os.Stderr, "stderr-payload")
		os.Exit(7)
	case "timeout":
		fmt.Fprint(os.Stdout, "before-timeout")
		fmt.Fprint(os.Stderr, "timeout-stderr")
		time.Sleep(30 * time.Second)
	case "environment":
		if os.Getenv("SANDBOX_ALLOWED") != "inherited-value" ||
			os.Getenv("SANDBOX_EXPLICIT") != "explicit-value" ||
			os.Getenv("SANDBOX_SECRET") != "" {
			fmt.Fprintf(os.Stderr, "environment contract violated: allowed=%q explicit=%q secret=%q",
				os.Getenv("SANDBOX_ALLOWED"), os.Getenv("SANDBOX_EXPLICIT"), os.Getenv("SANDBOX_SECRET"))
			os.Exit(9)
		}
		fmt.Fprint(os.Stdout, "environment-filtered")
	case "output":
		if delayMillis := helperIntegerEnvironment(helperDelayMillis); delayMillis > 0 {
			time.Sleep(time.Duration(delayMillis) * time.Millisecond)
		}
		stdoutBytes := helperIntegerEnvironment(helperStdoutBytes)
		stderrBytes := helperIntegerEnvironment(helperStderrBytes)
		if os.Getenv(helperInterleaved) == "1" {
			for stdoutBytes > 0 || stderrBytes > 0 {
				if stdoutBytes > 0 {
					writeHelperOutput(os.Stdout, 'O', 1)
					stdoutBytes--
				}
				if stderrBytes > 0 {
					writeHelperOutput(os.Stderr, 'E', 1)
					stderrBytes--
				}
			}
		} else {
			writeHelperOutput(os.Stdout, 'O', stdoutBytes)
			writeHelperOutput(os.Stderr, 'E', stderrBytes)
		}
		os.Exit(0)
	case "output-parent":
		child := exec.Command(os.Args[0], "-test.run=^TestSandboxHelperProcess$")
		child.Env = replaceSandboxHelperEnvironmentValue(os.Environ(), helperMode, "output-child")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "start output descendant: %v", err)
			os.Exit(12)
		}
		if err := waitForHelperPath(os.Getenv(helperReadyPath), 10*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "wait for output descendant: %v", err)
			os.Exit(13)
		}
		fmt.Fprintf(os.Stdout, "child-pid=%d\n", child.Process.Pid)
		if err := os.WriteFile(os.Getenv(helperStartPath), []byte("start"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "release output descendant: %v", err)
			os.Exit(14)
		}
		time.Sleep(30 * time.Second)
	case "output-child":
		if err := os.WriteFile(os.Getenv(helperReadyPath), []byte("ready"), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "signal output descendant: %v", err)
			os.Exit(15)
		}
		if err := waitForHelperPath(os.Getenv(helperStartPath), 10*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "wait for output release: %v", err)
			os.Exit(16)
		}
		for {
			writeHelperOutput(os.Stdout, 'D', 4096)
		}
	default:
		fmt.Fprint(os.Stderr, "unknown helper mode")
		os.Exit(10)
	}
}

func helperIntegerEnvironment(name string) int {
	value := os.Getenv(name)
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		fmt.Fprintf(os.Stderr, "invalid helper integer %s=%q", name, value)
		os.Exit(11)
	}
	return parsed
}

func writeHelperOutput(writer io.Writer, value byte, count int) {
	chunk := make([]byte, 4096)
	for index := range chunk {
		chunk[index] = value
	}
	for count > 0 {
		writeCount := len(chunk)
		if count < writeCount {
			writeCount = count
		}
		if _, err := writer.Write(chunk[:writeCount]); err != nil {
			return
		}
		count -= writeCount
	}
}

func waitForHelperPath(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %q", path)
		}
		time.Sleep(time.Millisecond)
	}
}

func replaceSandboxHelperEnvironmentValue(environment []string, name, value string) []string {
	prefix := strings.ToUpper(name) + "="
	replaced := false
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if strings.HasPrefix(strings.ToUpper(item), prefix) {
			result = append(result, name+"="+value)
			replaced = true
		} else {
			result = append(result, item)
		}
	}
	if !replaced {
		result = append(result, name+"="+value)
	}
	return result
}

func helperCommand(mode string) CommandSpec {
	executable, err := filepath.Abs(os.Args[0])
	if err != nil {
		executable = os.Args[0]
	}
	return CommandSpec{
		Path: executable,
		Args: []string{"-test.run=^TestSandboxHelperProcess$"},
		Environment: EnvironmentSpec{
			Inherit: essentialEnvironmentNames(),
			Set: map[string]string{
				helperEnabled: "1",
				helperMode:    mode,
			},
		},
	}
}

func essentialEnvironmentNames() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	return []string{"SYSTEMROOT", "WINDIR", "TEMP", "TMP"}
}
