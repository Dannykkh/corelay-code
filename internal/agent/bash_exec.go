package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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
	ExitCode      int                  `json:"exitCode"`
	IsError       bool                 `json:"isError"`
	Duration      time.Duration        `json:"duration"`
	Truncated     bool                 `json:"truncated"`
	TimedOut      bool                 `json:"timedOut"`
	Backgrounded  bool                 `json:"backgrounded"`
	SecurityBlock *SecurityCheckResult `json:"securityBlock,omitempty"`
}

// BashProgressCallback receives streaming output updates.
type BashProgressCallback func(output string, totalBytes int)

// ExecuteBashDeep is the deep implementation of bash execution.
// Includes: security validation, streaming output, timeout, auto-background, exit code semantics.
func ExecuteBashDeep(input json.RawMessage, workDir string, progressCb BashProgressCallback) BashExecResult {
	var args struct {
		Command     string            `json:"command"`
		Timeout     int               `json:"timeout"`
		Env         map[string]string `json:"env"`
		Description string            `json:"description"`
		Background  bool              `json:"run_in_background"`
	}
	json.Unmarshal(input, &args)

	command := strings.TrimSpace(args.Command)
	if command == "" {
		return BashExecResult{Output: "Empty command", IsError: true}
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
		return BashExecResult{
			Output:        fmt.Sprintf("[SECURITY] %s", secResult.Reason),
			IsError:       true,
			SecurityBlock: secResult,
		}
	}

	// ── Phase 2: Sleep detection ──
	if blocked := detectBlockedSleep(command); blocked != "" {
		return BashExecResult{
			Output:  blocked,
			IsError: true,
		}
	}

	// ── Phase 3: Timeout setup ──
	timeout := defaultBashTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// ── Phase 4: Execute with streaming ──
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	for k, v := range args.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	start := time.Now()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return BashExecResult{Output: err.Error(), IsError: true, Duration: time.Since(start)}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return BashExecResult{Output: err.Error(), IsError: true, Duration: time.Since(start)}
	}

	if err := cmd.Start(); err != nil {
		return BashExecResult{Output: err.Error(), IsError: true, Duration: time.Since(start)}
	}

	// Auto-background timer
	var backgrounded atomic.Bool
	bgTimer := time.NewTimer(autoBackgroundAfter)
	bgDone := make(chan struct{})
	defer func() {
		if !bgTimer.Stop() {
			select {
			case <-bgTimer.C:
			default:
			}
		}
		close(bgDone)
	}()

	go func() {
		select {
		case <-bgTimer.C:
			if cmd.Process != nil && cmd.ProcessState == nil {
				backgrounded.Store(true)
				log.Printf("[Bash] Auto-backgrounded after %v: %s", autoBackgroundAfter, truncateForDisplay(command, 80))
			}
		case <-bgDone:
		}
	}()

	// Stream output
	var outputBuf strings.Builder
	var totalBytes int
	var mu sync.Mutex
	truncated := false

	streamReader := func(r io.Reader, prefix string) {
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			if totalBytes < maxOutputBytes {
				if prefix != "" {
					outputBuf.WriteString(prefix)
				}
				outputBuf.WriteString(line)
				outputBuf.WriteString("\n")
			} else if !truncated {
				truncated = true
				outputBuf.WriteString("\n... (output truncated) ...\n")
			}
			totalBytes += len(line) + 1
			currentOutput := outputBuf.String()
			currentTotal := totalBytes
			mu.Unlock()

			if progressCb != nil {
				progressCb(currentOutput, currentTotal)
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); streamReader(stdout, "") }()
	go func() { defer wg.Done(); streamReader(stderr, "") }()

	// Wait for output streams to finish
	wg.Wait()
	err = cmd.Wait()
	duration := time.Since(start)

	// ── Phase 5: Build result ──
	result := BashExecResult{
		Duration:  duration,
		Truncated: truncated,
	}

	mu.Lock()
	output := outputBuf.String()
	mu.Unlock()

	// Truncate if still too large
	if len(output) > maxOutputBytes {
		output = output[:outputHeadKeep] + "\n\n... (middle truncated) ...\n\n" + output[len(output)-outputTailKeep:]
		result.Truncated = true
	}

	// ── Phase 6: Exit code interpretation ──
	exitCode := 0
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			result.TimedOut = true
			result.IsError = true
			output += fmt.Sprintf("\n[TIMEOUT after %ds]", int(timeout.Seconds()))
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	result.ExitCode = exitCode

	// Apply exit code semantics
	baseCmd := ParseBaseCommand(command)
	if exitCode != 0 && !result.TimedOut {
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
		output += fmt.Sprintf("\n[backgrounded after %v]", autoBackgroundAfter)
	}
	output += fmt.Sprintf("\n[%s | %.1fs]", filepath.Base(workDir), duration.Seconds())

	result.Output = output
	return result
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
