package sandbox

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestBoundedOutputCaptureUsesOneExactCombinedBudget(t *testing.T) {
	var limitSignals atomic.Int32
	capture := newBoundedOutputCapture(10, func() { limitSignals.Add(1) })
	if _, err := capture.stdoutWriter().Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.stderrWriter().Write([]byte("123456")); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, truncated := capture.snapshot()
	if truncated || string(stdout) != "abcd" || string(stderr) != "123456" || limitSignals.Load() != 0 {
		t.Fatalf("exact boundary stdout=%q stderr=%q truncated=%t signals=%d", stdout, stderr, truncated, limitSignals.Load())
	}
	if _, err := capture.stdoutWriter().Write([]byte("overflow")); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, truncated = capture.snapshot()
	if !truncated || len(stdout)+len(stderr) != 10 || limitSignals.Load() != 1 {
		t.Fatalf("overflow stdout=%q stderr=%q truncated=%t signals=%d", stdout, stderr, truncated, limitSignals.Load())
	}
}

func TestUnconfinedRunnerEnforcesCombinedOutputLimit(t *testing.T) {
	tests := []struct {
		name        string
		stdoutBytes int
		stderrBytes int
		interleaved bool
		limit       int64
		truncated   bool
	}{
		{name: "stdout only", stdoutBytes: 65, limit: 64, truncated: true},
		{name: "stderr only", stderrBytes: 65, limit: 64, truncated: true},
		{name: "interleaved combined", stdoutBytes: 80, stderrBytes: 80, interleaved: true, limit: 97, truncated: true},
		{name: "exact combined boundary", stdoutBytes: 32, stderrBytes: 32, interleaved: true, limit: 64},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := outputHelperCommand(test.stdoutBytes, test.stderrBytes, test.interleaved, 0)
			command.OutputLimitBytes = test.limit
			result, report := NewUnconfinedRunner().Run(context.Background(), Policy{Enforcement: EnforcementDisabled}, command)
			if !result.Started || len(result.Stdout)+len(result.Stderr) != int(test.limit) {
				t.Fatalf("result=%#v report=%#v stdout=%d stderr=%d", result, report, len(result.Stdout), len(result.Stderr))
			}
			if test.truncated {
				assertOutputLimitFailure(t, result, report, test.limit)
				return
			}
			if result.OutputTruncated || result.Err != nil || report.Failure != FailureNone || result.ExitCode != 0 {
				t.Fatalf("exact boundary result=%#v report=%#v", result, report)
			}
		})
	}
}

func TestUnconfinedRunnerZeroOutputLimitUsesSafeDefault(t *testing.T) {
	command := outputHelperCommand(int(DefaultOutputLimitBytes)+1, 0, false, 0)
	result, report := NewUnconfinedRunner().Run(context.Background(), Policy{Enforcement: EnforcementDisabled}, command)
	assertOutputLimitFailure(t, result, report, DefaultOutputLimitBytes)
	if len(result.Stdout) != int(DefaultOutputLimitBytes) || len(result.Stderr) != 0 {
		t.Fatalf("default capture sizes stdout=%d stderr=%d", len(result.Stdout), len(result.Stderr))
	}
}

func TestRunnerRejectsInvalidOutputLimitsBeforeStart(t *testing.T) {
	for _, limit := range []int64{-1, MaximumOutputLimitBytes + 1} {
		t.Run(strconv.FormatInt(limit, 10), func(t *testing.T) {
			command := helperCommand("success")
			command.OutputLimitBytes = limit
			result, report := NewUnconfinedRunner().Run(context.Background(), Policy{Enforcement: EnforcementDisabled}, command)
			if result.Started || report.Started || report.Failure != FailureCommandInvalid {
				t.Fatalf("limit=%d result=%#v report=%#v", limit, result, report)
			}
		})
	}
	if err := validateCommand(CommandSpec{Path: "fixture", OutputLimitBytes: MaximumOutputLimitBytes}); err != nil {
		t.Fatalf("maximum output limit was rejected: %v", err)
	}
}

func TestWrappedSandboxRunnerPreservesOutputLimitContract(t *testing.T) {
	runner := &wrappedSandboxRunner{
		name:         "fixture-secure",
		capabilities: Capabilities{FilesystemIsolation: true, ProcessTreeKill: platformProcessTreeKillSupported()},
		host:         NewUnconfinedRunner(),
		build: func(_ Policy, command CommandSpec) (preparedAdapterCommand, error) {
			return preparedAdapterCommand{
				Command:          command,
				AppliedIsolation: Capabilities{FilesystemIsolation: true},
			}, nil
		},
	}
	command := outputHelperCommand(512, 512, true, 0)
	command.OutputLimitBytes = 128
	result, report := runner.Run(context.Background(), Policy{Enforcement: EnforcementRequired}, command)
	assertOutputLimitFailure(t, result, report, command.OutputLimitBytes)
	if report.Runner != runner.Name() || !report.AppliedIsolation.FilesystemIsolation {
		t.Fatalf("wrapped report=%#v", report)
	}
}

func TestWrappedSandboxRunnerRejectsInvalidOutputLimitBeforeBuild(t *testing.T) {
	built := false
	runner := &wrappedSandboxRunner{
		name:         "fixture-secure",
		capabilities: Capabilities{FilesystemIsolation: true},
		host:         NewUnconfinedRunner(),
		build: func(_ Policy, command CommandSpec) (preparedAdapterCommand, error) {
			built = true
			return preparedAdapterCommand{Command: command}, nil
		},
	}
	command := helperCommand("success")
	command.OutputLimitBytes = MaximumOutputLimitBytes + 1
	result, report := runner.Run(context.Background(), Policy{Enforcement: EnforcementRequired}, command)
	if built || result.Started || report.Failure != FailureCommandInvalid {
		t.Fatalf("built=%t result=%#v report=%#v", built, result, report)
	}
}

func TestUnconfinedRunnerTimeoutOutputLimitRaceHasOneTerminalReason(t *testing.T) {
	for iteration := 0; iteration < 6; iteration++ {
		command := outputHelperCommand(4096, 0, false, 75*time.Millisecond)
		command.OutputLimitBytes = 32
		command.Timeout = 75 * time.Millisecond
		result, report := NewUnconfinedRunner().Run(context.Background(), Policy{Enforcement: EnforcementDisabled}, command)
		if !result.Started || result.Canceled {
			t.Fatalf("iteration=%d result=%#v report=%#v", iteration, result, report)
		}
		switch report.Failure {
		case FailureOutputLimit:
			if result.TimedOut || !result.OutputTruncated {
				t.Fatalf("output limit was not exclusive: iteration=%d result=%#v report=%#v", iteration, result, report)
			}
		case FailureTimedOut:
			if !result.TimedOut {
				t.Fatalf("timeout was not exclusive: iteration=%d result=%#v report=%#v", iteration, result, report)
			}
		default:
			t.Fatalf("iteration=%d failure=%q result=%#v report=%#v", iteration, report.Failure, result, report)
		}
	}
}

func TestUnconfinedRunnerCancellationOutputLimitRaceHasOneTerminalReason(t *testing.T) {
	for iteration := 0; iteration < 6; iteration++ {
		command := outputHelperCommand(4096, 0, false, 75*time.Millisecond)
		command.OutputLimitBytes = 32
		ctx, cancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(75*time.Millisecond, cancel)
		result, report := NewUnconfinedRunner().Run(ctx, Policy{Enforcement: EnforcementDisabled}, command)
		timer.Stop()
		cancel()
		if !result.Started || result.TimedOut {
			t.Fatalf("iteration=%d result=%#v report=%#v", iteration, result, report)
		}
		switch report.Failure {
		case FailureOutputLimit:
			if result.Canceled || !result.OutputTruncated {
				t.Fatalf("output limit was not exclusive: iteration=%d result=%#v report=%#v", iteration, result, report)
			}
		case FailureCanceled:
			if !result.Canceled {
				t.Fatalf("cancellation was not exclusive: iteration=%d result=%#v report=%#v", iteration, result, report)
			}
		default:
			t.Fatalf("iteration=%d failure=%q result=%#v report=%#v", iteration, report.Failure, result, report)
		}
	}
}

func assertOutputLimitFailure(t *testing.T, result Result, report Report, limit int64) {
	t.Helper()
	if !result.Started || !result.OutputTruncated || result.TimedOut || result.Canceled || report.Failure != FailureOutputLimit {
		t.Fatalf("limit=%d result=%#v report=%#v", limit, result, report)
	}
	if int64(len(result.Stdout)+len(result.Stderr)) != limit {
		t.Fatalf("captured=%d want=%d stdout=%d stderr=%d", len(result.Stdout)+len(result.Stderr), limit, len(result.Stdout), len(result.Stderr))
	}
	var runnerErr *RunnerError
	if !errors.As(result.Err, &runnerErr) || runnerErr.Code != FailureOutputLimit {
		t.Fatalf("error=%T %v", result.Err, result.Err)
	}
}

func outputHelperCommand(stdoutBytes, stderrBytes int, interleaved bool, delay time.Duration) CommandSpec {
	command := helperCommand("output")
	command.Environment.Set[helperStdoutBytes] = strconv.Itoa(stdoutBytes)
	command.Environment.Set[helperStderrBytes] = strconv.Itoa(stderrBytes)
	if interleaved {
		command.Environment.Set[helperInterleaved] = "1"
	}
	if delay > 0 {
		command.Environment.Set[helperDelayMillis] = strconv.FormatInt(delay.Milliseconds(), 10)
	}
	return command
}

func TestBoundedOutputCapturePreservesStreamPrefixes(t *testing.T) {
	capture := newBoundedOutputCapture(8, nil)
	_, _ = capture.stdoutWriter().Write(bytes.Repeat([]byte{'O'}, 5))
	_, _ = capture.stderrWriter().Write(bytes.Repeat([]byte{'E'}, 5))
	stdout, stderr, truncated := capture.snapshot()
	if !truncated || string(stdout) != "OOOOO" || string(stderr) != "EEE" {
		t.Fatalf("stdout=%q stderr=%q truncated=%t", stdout, stderr, truncated)
	}
}

func TestExecutionTerminalReasonIsImmutable(t *testing.T) {
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	contextFirst := &executionTerminal{}
	if contextFirst.claimOutputLimit(canceledContext) || contextFirst.load() != executionTerminalCanceled {
		t.Fatalf("context-first reason=%v", contextFirst.load())
	}

	activeContext, cancelActive := context.WithCancel(context.Background())
	outputFirst := &executionTerminal{}
	if !outputFirst.claimOutputLimit(activeContext) {
		t.Fatal("output limit did not claim empty terminal state")
	}
	cancelActive()
	outputFirst.claimContext(activeContext)
	if outputFirst.load() != executionTerminalOutputLimit {
		t.Fatalf("output-first reason=%v", outputFirst.load())
	}
}
