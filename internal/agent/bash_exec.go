package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const (
	defaultBashTimeout  = 120 * time.Second
	maxBashTimeout      = 600 * time.Second
	autoBackgroundAfter = 15 * time.Second
	maxOutputBytes      = 100000
	outputTailKeep      = 10000 // keep last 10KB when truncating
	outputHeadKeep      = 50000 // keep first 50KB when truncating
	sleepBlockThreshold = 300   // block sleep > 5 minutes
)

// BashExecResult holds the complete result of a bash execution.
type BashExecResult struct {
	Output        string               `json:"output"`
	Stdout        string               `json:"stdout,omitempty"`
	Stderr        string               `json:"stderr,omitempty"`
	ExitCode      int                  `json:"exitCode"`
	IsError       bool                 `json:"isError"`
	Duration      time.Duration        `json:"duration"`
	Truncated     bool                 `json:"truncated"`
	TimedOut      bool                 `json:"timedOut"`
	Canceled      bool                 `json:"canceled"`
	Backgrounded  bool                 `json:"backgrounded"`
	SecurityBlock *SecurityCheckResult `json:"securityBlock,omitempty"`
	SandboxReport sandbox.Report       `json:"sandbox"`
}

// BashProgressCallback receives the bounded captured output and original byte
// count. Runner implementations retain ownership of process I/O collection.
type BashProgressCallback func(output string, totalBytes int)

// ExecuteBashDeep is the deep implementation of bash execution.
// Includes: security validation, bounded output, timeout, auto-background, exit code semantics.
func ExecuteBashDeep(input json.RawMessage, workDir string, progressCb BashProgressCallback) BashExecResult {
	return ExecuteBashDeepWithOptions(input, workDir, legacyUnconfinedBashOptions(progressCb))
}

// ExecuteBashDeepWithOptions executes Bash through the configured sandbox
// Runner. Unlike the legacy wrapper, its zero value fails before process start.
func ExecuteBashDeepWithOptions(input json.RawMessage, workDir string, opts BashExecOptions) BashExecResult {
	var args struct {
		Command     string            `json:"command"`
		Timeout     int               `json:"timeout"`
		Env         map[string]string `json:"env"`
		Description string            `json:"description"`
		Background  bool              `json:"run_in_background"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return bashSetupFailure(opts, sandbox.FailureCommandInvalid, "Invalid Bash input: "+err.Error())
	}

	command := strings.TrimSpace(args.Command)
	if command == "" {
		return bashSetupFailure(opts, sandbox.FailureCommandInvalid, "Empty command")
	}

	// ── Phase 1: Security validation ──
	// First check the full command (catches cross-pipe patterns like "curl | bash")
	secResult := ValidateBashSecurity(command, workDir)
	// Then check each subcommand individually
	if secResult == nil && isCompoundCommand(command) {
		secResult = ValidateCompoundCommand(command, workDir)
	}
	if secResult != nil {
		log.Printf("[Bash] BLOCKED: %s — %s", secResult.Pattern, secResult.Reason)
		result := bashSetupFailure(opts, sandbox.FailureCommandInvalid, secResult.Reason)
		result.Output = fmt.Sprintf("[SECURITY] %s", secResult.Reason)
		result.SecurityBlock = secResult
		return result
	}

	// ── Phase 2: Sleep detection ──
	if blocked := detectBlockedSleep(command); blocked != "" {
		result := bashSetupFailure(opts, sandbox.FailureCommandInvalid, blocked)
		result.Output = blocked
		return result
	}

	if opts.Runner == nil {
		return bashSetupFailure(opts, sandbox.FailureRunnerUnavailable, "sandbox runner is not configured")
	}
	if err := sandbox.ValidatePolicy(opts.Policy, opts.Runner.Capabilities()); err != nil {
		failure := sandbox.FailurePolicyInvalid
		var policyError *sandbox.PolicyError
		if errors.As(err, &policyError) {
			failure = policyError.Code
		}
		return bashSetupFailure(opts, failure, err.Error())
	}
	environment, err := bashEnvironmentSpec(args.Env)
	if err != nil {
		return bashSetupFailure(opts, sandbox.FailureCommandInvalid, err.Error())
	}

	// ── Phase 3: Timeout setup ──
	timeout := defaultBashTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}

	parentContext := opts.Context
	if parentContext == nil {
		parentContext = context.Background()
	}
	ctx, cancel := context.WithTimeout(parentContext, timeout)
	defer cancel()

	// ── Phase 4: Execute through the selected Runner ──
	start := time.Now()
	var backgrounded atomic.Bool
	bgTimer := time.AfterFunc(autoBackgroundAfter, func() {
		backgrounded.Store(true)
		log.Printf("[Bash] Auto-backgrounded after %v: %s", autoBackgroundAfter, truncateForDisplay(command, 80))
	})
	runnerResult, report := opts.Runner.Run(ctx, opts.Policy, sandbox.CommandSpec{
		Path:        "bash",
		Args:        []string{"-c", command},
		Dir:         workDir,
		Environment: environment,
		Timeout:     timeout,
	})
	bgTimer.Stop()
	if opts.ObserveReport != nil {
		opts.ObserveReport(report)
	}
	duration := runnerResult.Duration
	if duration <= 0 {
		duration = time.Since(start)
	}

	// ── Phase 5: Build result ──
	result := BashExecResult{
		Stdout:        string(runnerResult.Stdout),
		Stderr:        string(runnerResult.Stderr),
		ExitCode:      runnerResult.ExitCode,
		Duration:      duration,
		TimedOut:      runnerResult.TimedOut,
		Canceled:      runnerResult.Canceled,
		SandboxReport: report,
	}
	output := combineBashOutput(result.Stdout, result.Stderr)
	totalOutputBytes := len(output)
	output, result.Truncated = truncateBashOutput(output)
	if opts.Progress != nil && output != "" {
		opts.Progress(output, totalOutputBytes)
	}

	if !runnerResult.Started {
		result.IsError = true
		if strings.TrimSpace(report.Detail) != "" {
			output = appendBashLine(output, "[SANDBOX] "+report.Detail)
		} else if runnerResult.Err != nil {
			output = appendBashLine(output, "[SANDBOX] "+runnerResult.Err.Error())
		} else {
			output = appendBashLine(output, "[SANDBOX] process did not start")
		}
		result.Output = appendBashFooter(output, workDir, duration, report)
		return result
	}

	// ── Phase 6: Exit code interpretation ──
	exitCode := runnerResult.ExitCode
	if result.TimedOut {
		result.IsError = true
		output = appendBashLine(output, fmt.Sprintf("[TIMEOUT after %ds]", int(timeout.Seconds())))
	} else if result.Canceled {
		result.IsError = true
		output = appendBashLine(output, "[CANCELED]")
	} else if runnerResult.Err != nil && exitCode == 0 {
		result.IsError = true
		output = appendBashLine(output, "[EXECUTION ERROR] "+runnerResult.Err.Error())
	}

	result.ExitCode = exitCode

	// Apply exit code semantics
	baseCmd := ParseBaseCommand(command)
	if exitCode != 0 && !result.TimedOut && !result.Canceled {
		semantic, isRealError := ExitCodeSemantics(baseCmd, exitCode)
		if !isRealError {
			result.IsError = false
			if semantic != "" {
				output += fmt.Sprintf("\n[exit %d: %s]", exitCode, semantic)
			}
		} else {
			result.IsError = true
			output += fmt.Sprintf("\n[exit %d]", exitCode)

			// ── Phase 7: Error recovery hints ──
			hints := getRecoveryHints(command, output, workDir)
			if hints != "" {
				output += "\n" + hints
			}
		}
	}

	// Add footer
	if backgrounded.Load() {
		result.Backgrounded = true
		output = appendBashLine(output, fmt.Sprintf("[backgrounded after %v]", autoBackgroundAfter))
	}
	result.Output = appendBashFooter(output, workDir, duration, report)
	return result
}

func bashSetupFailure(opts BashExecOptions, code sandbox.FailureCode, detail string) BashExecResult {
	report := sandbox.Report{
		Runner:               "unconfigured",
		RequestedEnforcement: opts.Policy.Enforcement,
		Failure:              code,
		Detail:               detail,
	}
	if opts.Runner != nil {
		report.Runner = opts.Runner.Name()
		report.Capabilities = opts.Runner.Capabilities()
	}
	if opts.ObserveReport != nil {
		opts.ObserveReport(report)
	}
	return BashExecResult{
		Output:        "[SANDBOX] " + detail,
		ExitCode:      sandbox.ExitNotStarted,
		IsError:       true,
		SandboxReport: report,
	}
}

func combineBashOutput(stdout, stderr string) string {
	if stdout == "" {
		return stderr
	}
	if stderr == "" {
		return stdout
	}
	if strings.HasSuffix(stdout, "\n") {
		return stdout + stderr
	}
	return stdout + "\n" + stderr
}

func truncateBashOutput(output string) (string, bool) {
	if len(output) <= maxOutputBytes {
		return output, false
	}
	return output[:outputHeadKeep] + "\n\n... (middle truncated) ...\n\n" + output[len(output)-outputTailKeep:], true
}

func appendBashLine(output, line string) string {
	if output == "" {
		return line
	}
	if strings.HasSuffix(output, "\n") {
		return output + line
	}
	return output + "\n" + line
}

func appendBashFooter(output, workDir string, duration time.Duration, report sandbox.Report) string {
	effective := string(report.EffectiveEnforcement)
	if effective == "" {
		effective = "not-started"
	}
	sandboxLine := fmt.Sprintf(
		"[sandbox: %s | requested=%s | effective=%s",
		report.Runner,
		report.RequestedEnforcement,
		effective,
	)
	if report.Failure != sandbox.FailureNone {
		sandboxLine += " | status=" + string(report.Failure)
	}
	output = appendBashLine(output, sandboxLine+"]")
	return appendBashLine(output, fmt.Sprintf("[%s | %.1fs]", filepath.Base(workDir), duration.Seconds()))
}

func truncateForDisplay(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// detectBlockedSleep checks for long sleep commands.
func detectBlockedSleep(cmd string) string {
	// Match sleep with large values
	patterns := []struct {
		re  string
		max int
	}{
		{`sleep\s+(\d+)`, sleepBlockThreshold},
	}

	for _, p := range patterns {
		re := regexp.MustCompile(p.re)
		matches := re.FindStringSubmatch(cmd)
		if len(matches) > 1 {
			var seconds int
			fmt.Sscanf(matches[1], "%d", &seconds)
			if seconds > p.max {
				return fmt.Sprintf("[BLOCKED] sleep %d seconds is too long. Use background execution or reduce duration.", seconds)
			}
		}
	}
	return ""
}

// getRecoveryHints provides actionable hints for common errors.
func getRecoveryHints(command, output, workDir string) string {
	var hints []string

	// Git index.lock
	if strings.Contains(output, "index.lock") {
		lockPath := filepath.Join(workDir, ".git", "index.lock")
		if _, err := os.Stat(lockPath); err == nil {
			hints = append(hints, fmt.Sprintf("Hint: git index.lock exists. Run: rm %s", lockPath))
		}
	}

	// Permission denied
	if strings.Contains(output, "Permission denied") {
		hints = append(hints, "Hint: Permission denied. Check file permissions or try with appropriate access.")
	}

	// Command not found
	if strings.Contains(output, "command not found") || strings.Contains(output, "not recognized") {
		base := ParseBaseCommand(command)
		hints = append(hints, fmt.Sprintf("Hint: '%s' not found. Check if it's installed and in PATH.", base))
	}

	// Port already in use
	if strings.Contains(output, "address already in use") || strings.Contains(output, "EADDRINUSE") {
		hints = append(hints, "Hint: Port already in use. Check with: netstat -tlnp | grep <port>")
	}

	// npm/node errors
	if strings.Contains(output, "ENOENT") && strings.Contains(command, "npm") {
		if _, err := os.Stat(filepath.Join(workDir, "package.json")); os.IsNotExist(err) {
			hints = append(hints, "Hint: No package.json found. Run: npm init")
		}
	}

	if len(hints) == 0 {
		return ""
	}
	return strings.Join(hints, "\n")
}
