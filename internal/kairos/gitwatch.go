package kairos

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const defaultGitWatchTimeout = 5 * time.Second

// GitExecutionOptions binds KAIROS' fixed, read-only Git probes to the same
// supervised process boundary used by the agent runtime. The zero value is
// invalid; CheckGitStatus supplies the secure application default.
type GitExecutionOptions struct {
	Context       context.Context
	Runner        sandbox.Runner
	Policy        sandbox.Policy
	Timeout       time.Duration
	ObserveReport func(sandbox.Report)
}

// GitStatus holds the current git state of a project.
type GitStatus struct {
	Branch       string   `json:"branch"`
	Staged       []string `json:"staged"`
	Modified     []string `json:"modified"`
	Untracked    []string `json:"untracked"`
	AheadBy      int      `json:"aheadBy"`
	LastCommit   string   `json:"lastCommit"`
	LastCommitAt string   `json:"lastCommitAt"`
}

// CheckGitStatus runs git commands in the given directory and returns status.
func CheckGitStatus(workDir string) (*GitStatus, error) {
	return CheckGitStatusWithOptions(workDir, defaultGitExecution(workDir))
}

// CheckGitStatusWithOptions runs only fixed read-only Git argv through an
// explicitly supplied sandbox boundary. No model or project string is ever
// used as an executable or argument.
func CheckGitStatusWithOptions(workDir string, options GitExecutionOptions) (*GitStatus, error) {
	if workDir == "" {
		return nil, fmt.Errorf("no workDir")
	}
	canonicalWorkDir, err := canonicalGitWorkDir(workDir)
	if err != nil {
		return nil, err
	}
	options, err = resolveGitExecution(options, canonicalWorkDir)
	if err != nil {
		return nil, err
	}

	// Check if it's a git repo
	if _, err := gitOutput(options, canonicalWorkDir, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("not a git repo")
	}

	status := &GitStatus{}

	// Branch name
	if out, err := gitOutput(options, canonicalWorkDir, "branch", "--show-current"); err == nil {
		status.Branch = strings.TrimSpace(out)
	}

	// Porcelain status
	if out, err := gitOutput(options, canonicalWorkDir, "status", "--porcelain=v1", "--untracked-files=normal"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimRight(line, "\r")
			if len(line) < 3 {
				continue
			}
			xy := line[:2]
			file := sanitizeGitDisplayPath(strings.TrimSpace(line[2:]))
			if file == "" {
				continue
			}
			// Rename arrows
			if idx := strings.Index(file, " -> "); idx >= 0 {
				file = file[idx+4:]
			}

			if xy[0] != ' ' && xy[0] != '?' {
				status.Staged = append(status.Staged, file)
			}
			if xy[1] == 'M' || xy[1] == 'D' {
				status.Modified = append(status.Modified, file)
			}
			if xy == "??" {
				status.Untracked = append(status.Untracked, file)
			}
		}
	}

	// Ahead count
	if out, err := gitOutput(options, canonicalWorkDir, "rev-list", "--count", "@{u}..HEAD"); err == nil {
		fmt.Sscanf(strings.TrimSpace(out), "%d", &status.AheadBy)
	}

	// Last commit
	if out, err := gitOutput(options, canonicalWorkDir, "log", "-1", "--format=%s|||%ci"); err == nil {
		parts := strings.SplitN(strings.TrimSpace(out), "|||", 2)
		if len(parts) == 2 {
			status.LastCommit = parts[0]
			status.LastCommitAt = parts[1]
		}
	}

	return status, nil
}

// GitWatchSummary creates a human-readable summary of git changes.
func GitWatchSummary(prev, curr *GitStatus) string {
	if curr == nil {
		return "Not a git repository"
	}

	var parts []string

	total := len(curr.Staged) + len(curr.Modified) + len(curr.Untracked)
	if total == 0 {
		return fmt.Sprintf("[%s] Clean working tree. Last: %s", curr.Branch, curr.LastCommit)
	}

	parts = append(parts, fmt.Sprintf("[%s]", curr.Branch))

	if len(curr.Staged) > 0 {
		parts = append(parts, fmt.Sprintf("staged:%d", len(curr.Staged)))
	}
	if len(curr.Modified) > 0 {
		parts = append(parts, fmt.Sprintf("modified:%d(%s)", len(curr.Modified), joinMax(curr.Modified, 3)))
	}
	if len(curr.Untracked) > 0 {
		parts = append(parts, fmt.Sprintf("untracked:%d", len(curr.Untracked)))
	}
	if curr.AheadBy > 0 {
		parts = append(parts, fmt.Sprintf("ahead:%d (unpushed)", curr.AheadBy))
	}

	// Detect new changes since previous check
	if prev != nil {
		newMod := diffSlice(curr.Modified, prev.Modified)
		if len(newMod) > 0 {
			parts = append(parts, fmt.Sprintf("NEW: %s", joinMax(newMod, 5)))
		}
	}

	return strings.Join(parts, " ")
}

// ── Helpers ──

func gitOutput(options GitExecutionOptions, dir string, args ...string) (string, error) {
	result, report := options.Runner.Run(options.Context, options.Policy, sandbox.CommandSpec{
		Path: "git",
		Args: append([]string(nil), args...),
		Dir:  dir,
		Environment: sandbox.EnvironmentSpec{
			Inherit: gitInheritedEnvironment(),
		},
		Timeout: options.Timeout,
	})
	if options.ObserveReport != nil {
		options.ObserveReport(report)
	}
	if report.Failure != sandbox.FailureNone || !result.Started {
		return "", fmt.Errorf("git probe failed: %s", safeGitFailure(report.Failure))
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("git probe exited non-zero")
	}
	return string(result.Stdout), nil
}

func defaultGitExecution(workDir string) GitExecutionOptions {
	runner := sandbox.NewAutoRunner()
	capabilities := runner.Capabilities()
	policy := sandbox.Policy{
		Enforcement: sandbox.EnforcementPreferred,
		Required: sandbox.Capabilities{
			ProcessTreeKill:      true,
			EnvironmentFiltering: true,
			Timeouts:             true,
		},
	}
	if capabilities.FilesystemIsolation {
		policy.Workspace = workDir
		policy.WorkspaceAccess = sandbox.WorkspaceReadOnly
	}
	if capabilities.NetworkIsolation {
		policy.Network = sandbox.NetworkDenied
	}
	return GitExecutionOptions{
		Context: context.Background(), Runner: runner, Policy: policy,
		Timeout: defaultGitWatchTimeout,
	}
}

func resolveGitExecution(options GitExecutionOptions, workDir string) (GitExecutionOptions, error) {
	if options.Context == nil {
		options.Context = context.Background()
	}
	if options.Runner == nil || options.Policy.Enforcement == "" {
		return GitExecutionOptions{}, fmt.Errorf("git probe sandbox is not configured")
	}
	if options.Timeout <= 0 || options.Timeout > 30*time.Second {
		return GitExecutionOptions{}, fmt.Errorf("git probe timeout is invalid")
	}
	if options.Policy.WorkspaceAccess != sandbox.WorkspaceAccessUnspecified {
		options.Policy.Workspace = workDir
	}
	return options, nil
}

func canonicalGitWorkDir(workDir string) (string, error) {
	absolute, err := filepath.Abs(filepath.FromSlash(strings.TrimSpace(workDir)))
	if err != nil {
		return "", fmt.Errorf("invalid workDir")
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(absolute))
	if err != nil {
		return "", fmt.Errorf("invalid workDir")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("invalid workDir")
	}
	return canonical, nil
}

func gitInheritedEnvironment() []string {
	if runtime.GOOS == "windows" {
		return []string{"PATH", "PATHEXT", "SystemRoot", "WINDIR", "TEMP", "TMP"}
	}
	return []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "TZ"}
}

func safeGitFailure(code sandbox.FailureCode) string {
	if code == sandbox.FailureNone {
		return "execution_failed"
	}
	return string(code)
}

func sanitizeGitDisplayPath(value string) string {
	const maxDisplayBytes = 512
	var builder strings.Builder
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			builder.WriteByte('?')
		} else {
			builder.WriteRune(char)
		}
		if builder.Len() >= maxDisplayBytes {
			break
		}
	}
	return builder.String()
}

func joinMax(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(" +%d", len(items)-max)
}

func diffSlice(curr, prev []string) []string {
	prevSet := make(map[string]bool, len(prev))
	for _, p := range prev {
		prevSet[p] = true
	}
	var diff []string
	for _, c := range curr {
		if !prevSet[c] {
			diff = append(diff, c)
		}
	}
	return diff
}

// RunGitWatch is a built-in task that checks git status and logs changes.
func (d *Daemon) RunGitWatch() {
	d.mu.RLock()
	workDir := d.workDir
	d.mu.RUnlock()

	curr, err := CheckGitStatus(workDir)
	if err != nil {
		d.addLog("git-watch", err.Error())
		return
	}

	prev := d.lastGitStatus
	summary := GitWatchSummary(prev, curr)
	d.mu.Lock()
	d.lastGitStatus = curr
	d.mu.Unlock()

	d.addLog("git-watch", summary)

	// Notify if new changes detected
	totalPrev := 0
	if prev != nil {
		totalPrev = len(prev.Staged) + len(prev.Modified) + len(prev.Untracked)
	}
	totalCurr := len(curr.Staged) + len(curr.Modified) + len(curr.Untracked)
	if totalCurr > totalPrev && d.notifier != nil {
		d.notifier.Send(Notification{
			Type:    "git-change",
			Title:   "Git changes detected",
			Message: summary,
			Project: filepath.Base(workDir),
		})
	}
}

// LastGitStatus returns the most recent git status.
func (d *Daemon) LastGitStatus() *GitStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.lastGitStatus
}

// AutoGitWatch creates a default git-watch task.
func AutoGitWatchTask() Task {
	return Task{
		ID:          "git-watch",
		Type:        "git-watch",
		Description: "Monitor git changes",
		Enabled:     true,
		CreatedAt:   time.Now(),
	}
}
