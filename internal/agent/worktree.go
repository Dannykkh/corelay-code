package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const (
	managedWorktreeStateDir       = ".corelay"
	legacyManagedWorktreeStateDir = ".aniclew"
	proxyManagedWorktreeStateDir  = ".claude-proxy"
	claudeManagedWorktreeStateDir = ".claude"
	managedWorktreeDir            = ".worktrees"
	managedWorktreeBranchPrefix   = "corelaycode/"
)

// Worktree manages git worktree isolation for parallel agents.
type Worktree struct {
	Name      string `json:"name"`
	Path      string `json:"path"`      // absolute path to worktree
	Branch    string `json:"branch"`    // branch name
	ParentDir string `json:"parentDir"` // main repo dir
}

// WorktreeProcessError preserves the typed sandbox report for callers that
// need more than the human-readable git output.
type WorktreeProcessError struct {
	Operation string
	Output    string
	ExitCode  int
	Report    sandbox.Report
}

func (e *WorktreeProcessError) Error() string {
	detail := strings.TrimSpace(e.Output)
	if detail == "" {
		detail = strings.TrimSpace(e.Report.Detail)
	}
	if detail == "" {
		detail = fmt.Sprintf("git exited with code %d", e.ExitCode)
	}
	return e.Operation + ": " + detail
}

func defaultWorktreeToolOptions(repoDir string) ToolExecutionOptions {
	runner, policy := DefaultSandboxExecution(repoDir)
	return ToolExecutionOptions{
		Context:       context.Background(),
		SandboxRunner: runner,
		SandboxPolicy: policy,
	}
}

// CreateWorktree retains API compatibility while explicitly selecting the
// platform secure default. Production composition with cancellation/reporting
// should use CreateWorktreeWithOptions.
func CreateWorktree(repoDir string, name string) (*Worktree, error) {
	return CreateWorktreeWithOptions(repoDir, name, defaultWorktreeToolOptions(repoDir))
}

// CreateWorktreeWithOptions creates an isolated worktree through the common
// sandbox process boundary.
func CreateWorktreeWithOptions(repoDir string, name string, opts ToolExecutionOptions) (*Worktree, error) {
	repo, err := canonicalWorkspace(repoDir)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree repository: %w", err)
	}
	safeName := sanitizeName(name)
	if safeName == "" {
		return nil, fmt.Errorf("worktree name contains no safe characters")
	}

	if process := runWorktreeGit(opts, repo, "verify git repository", "rev-parse", "--git-dir"); worktreeProcessFailed(process) {
		return nil, newWorktreeProcessError("verify git repository", process)
	}

	worktreeBase, err := ensureManagedWorktreeBase(repo)
	if err != nil {
		return nil, err
	}
	worktreePath, err := canonicalManagedWorktreePath(repo, filepath.Join(worktreeBase, safeName))
	if err != nil {
		return nil, err
	}
	branchName := managedWorktreeBranchPrefix + safeName

	if _, statErr := os.Lstat(worktreePath); statErr == nil {
		if err := RemoveWorktreeWithOptions(repo, worktreePath, opts); err != nil {
			return nil, fmt.Errorf("remove existing managed worktree: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("inspect existing managed worktree: %w", statErr)
	}

	branch := runWorktreeGit(opts, repo, "inspect worktree branch", "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
	switch {
	case branch.Started && branch.ExitCode == 0 && branch.Err == nil:
		removeBranch := runWorktreeGit(opts, repo, "remove existing worktree branch", "branch", "-D", branchName)
		if worktreeProcessFailed(removeBranch) {
			return nil, newWorktreeProcessError("remove existing worktree branch", removeBranch)
		}
	case branch.Started && branch.ExitCode == 1:
		// show-ref exit 1 means the branch does not exist.
	default:
		return nil, newWorktreeProcessError("inspect worktree branch", branch)
	}

	add := runWorktreeGit(opts, repo, "create worktree", "worktree", "add", "-b", branchName, worktreePath)
	if worktreeProcessFailed(add) {
		return nil, newWorktreeProcessError("create worktree", add)
	}

	log.Printf("[Worktree] Created: %s at %s (branch: %s)", name, worktreePath, branchName)
	return &Worktree{
		Name:      name,
		Path:      worktreePath,
		Branch:    branchName,
		ParentDir: repo,
	}, nil
}

// HasChanges checks if the worktree has uncommitted changes using a secure
// default. Callers needing errors or a shared runner should use WithOptions.
func (w *Worktree) HasChanges() bool {
	changed, _ := w.HasChangesWithOptions(defaultWorktreeToolOptions(w.ParentDir))
	return changed
}

func (w *Worktree) HasChangesWithOptions(opts ToolExecutionOptions) (bool, error) {
	dir, err := canonicalKnownWorktreePath(w.ParentDir, w.Path)
	if err != nil {
		return false, err
	}
	process := runWorktreeGit(opts, dir, "inspect worktree status", "status", "--porcelain")
	if worktreeProcessFailed(process) {
		return false, newWorktreeProcessError("inspect worktree status", process)
	}
	return strings.TrimSpace(process.combinedOutput()) != "", nil
}

// GetDiff returns the diff summary using a secure default.
func (w *Worktree) GetDiff() string {
	diff, _ := w.GetDiffWithOptions(defaultWorktreeToolOptions(w.ParentDir))
	return diff
}

func (w *Worktree) GetDiffWithOptions(opts ToolExecutionOptions) (string, error) {
	dir, err := canonicalKnownWorktreePath(w.ParentDir, w.Path)
	if err != nil {
		return "", err
	}
	process := runWorktreeGit(opts, dir, "inspect worktree diff", "diff", "--stat")
	if worktreeProcessFailed(process) {
		return "", newWorktreeProcessError("inspect worktree diff", process)
	}
	return process.combinedOutput(), nil
}

// RemoveWorktree removes a managed worktree using a secure platform default.
func RemoveWorktree(repoDir string, worktreePath string) error {
	return RemoveWorktreeWithOptions(repoDir, worktreePath, defaultWorktreeToolOptions(repoDir))
}

func RemoveWorktreeWithOptions(repoDir string, worktreePath string, opts ToolExecutionOptions) error {
	repo, err := canonicalWorkspace(repoDir)
	if err != nil {
		return fmt.Errorf("resolve worktree repository: %w", err)
	}
	target, err := canonicalManagedWorktreePath(repo, worktreePath)
	if err != nil {
		return err
	}
	remove := runWorktreeGit(opts, repo, "remove worktree", "worktree", "remove", "--force", target)
	if worktreeProcessFailed(remove) {
		// Never fall back to host RemoveAll: a sandbox or git failure must remain
		// fail-closed at the process boundary.
		return newWorktreeProcessError("remove worktree", remove)
	}
	prune := runWorktreeGit(opts, repo, "prune worktrees", "worktree", "prune")
	if worktreeProcessFailed(prune) {
		return newWorktreeProcessError("prune worktrees", prune)
	}
	log.Printf("[Worktree] Removed: %s", target)
	return nil
}

// ListWorktrees preserves the legacy slice-only API with a secure default.
func ListWorktrees(repoDir string) []Worktree {
	worktrees, _ := ListWorktreesWithOptions(repoDir, defaultWorktreeToolOptions(repoDir))
	return worktrees
}

func ListWorktreesWithOptions(repoDir string, opts ToolExecutionOptions) ([]Worktree, error) {
	repo, err := canonicalWorkspace(repoDir)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree repository: %w", err)
	}
	process := runWorktreeGit(opts, repo, "list worktrees", "worktree", "list", "--porcelain")
	if worktreeProcessFailed(process) {
		return nil, newWorktreeProcessError("list worktrees", process)
	}

	var worktrees []Worktree
	var current Worktree
	for _, line := range strings.Split(process.combinedOutput(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if current.Path != "" {
				worktrees = append(worktrees, current)
			}
			current = Worktree{}
			continue
		}
		if strings.HasPrefix(line, "worktree ") {
			current.Path = line[9:]
			current.ParentDir = repo
		}
		if strings.HasPrefix(line, "branch ") {
			current.Branch = line[7:]
			current.Name = filepath.Base(current.Path)
		}
	}
	if current.Path != "" {
		worktrees = append(worktrees, current)
	}
	return worktrees, nil
}

// WorktreeNotice generates the context notice for a child agent in a worktree.
func WorktreeNotice(parentDir, worktreePath string) string {
	return fmt.Sprintf(`You are working in an isolated git worktree.
- Parent repo: %s
- Your worktree: %s
- Your changes are isolated and won't affect the parent's files.
- Re-read files before editing — they may differ from the parent context.
- When done, your changes can be reviewed and merged separately.`, parentDir, worktreePath)
}

func sanitizeName(name string) string {
	name = strings.ToLower(name)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, name)
	return strings.Trim(name, "-")
}

func runWorktreeGit(opts ToolExecutionOptions, dir, operation string, args ...string) toolProcessResult {
	return runToolProcess(opts, operation, dir, "git", args, defaultToolProcessTimeout)
}

func worktreeProcessFailed(process toolProcessResult) bool {
	return process.policyOrContextFailure() || !process.Started || process.ExitCode != 0 || process.Err != nil
}

func newWorktreeProcessError(operation string, process toolProcessResult) error {
	return &WorktreeProcessError{
		Operation: operation,
		Output:    process.combinedOutput(),
		ExitCode:  process.ExitCode,
		Report:    process.Report,
	}
}

func ensureManagedWorktreeBase(repo string) (string, error) {
	base := preferredManagedWorktreeBase(repo)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("create managed worktree directory: %w", err)
	}
	canonical, err := canonicalizeTarget(base)
	if err != nil {
		return "", fmt.Errorf("resolve managed worktree directory: %w", err)
	}
	if !pathWithin(canonical, repo) {
		return "", fmt.Errorf("managed worktree directory escapes repository: %s", canonical)
	}
	return canonical, nil
}

func preferredManagedWorktreeBase(repo string) string {
	current := filepath.Join(repo, managedWorktreeStateDir, managedWorktreeDir)
	if worktreeStatePathExists(current) {
		return current
	}
	for _, name := range []string{
		legacyManagedWorktreeStateDir,
		proxyManagedWorktreeStateDir,
		// Corelay Code placed managed worktrees under Claude's state directory
		// while it was named AniClew. Keep that location as the final fallback
		// so active legacy worktrees are not orphaned during the rename.
		claudeManagedWorktreeStateDir,
	} {
		legacy := filepath.Join(repo, name, managedWorktreeDir)
		if worktreeStatePathExists(legacy) {
			return legacy
		}
	}
	return current
}

func worktreeStatePathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !os.IsNotExist(err)
}

func canonicalManagedWorktreePath(repo, rawPath string) (string, error) {
	base, err := ensureManagedWorktreeBase(repo)
	if err != nil {
		return "", err
	}
	target := rawPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(repo, target)
	}
	target, err = canonicalizeTarget(target)
	if err != nil {
		return "", fmt.Errorf("resolve managed worktree path: %w", err)
	}
	if target == base || !pathWithin(target, base) {
		return "", fmt.Errorf("worktree path is outside managed scope: %s", rawPath)
	}
	return target, nil
}

func canonicalKnownWorktreePath(repoDir, worktreePath string) (string, error) {
	repo, err := canonicalWorkspace(repoDir)
	if err != nil {
		return "", fmt.Errorf("resolve worktree repository: %w", err)
	}
	target, err := canonicalizeTarget(worktreePath)
	if err != nil {
		return "", fmt.Errorf("resolve worktree path: %w", err)
	}
	if target == repo {
		return target, nil
	}
	return canonicalManagedWorktreePath(repo, target)
}
