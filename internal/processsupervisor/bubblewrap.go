package processsupervisor

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func newBubblewrapAdapter(dependencies AdapterDependencies) Runner {
	if dependencies.Host == nil || !dependencies.Host.Capabilities().ProcessTreeKill {
		return NewUnavailableRunner("bubblewrap streaming supervision requires process-tree termination")
	}
	path, err := dependencies.Lookup("bwrap")
	if err != nil {
		return NewUnavailableRunner(fmt.Sprintf("bubblewrap is unavailable: %v", err))
	}
	if err := dependencies.Probe(path, bubblewrapProbeArgs(false)); err != nil {
		return NewUnavailableRunner(fmt.Sprintf("bubblewrap capability probe failed: %v", err))
	}
	networkIsolation := dependencies.Probe(path, bubblewrapProbeArgs(true)) == nil
	capabilities := sandbox.Capabilities{
		FilesystemIsolation: true, NetworkIsolation: networkIsolation, ProcessIsolation: true,
		ProcessTreeKill: true, EnvironmentFiltering: true, Timeouts: true,
	}
	return &wrappedRunner{
		name: "bubblewrap", capabilities: capabilities, host: dependencies.Host,
		build: func(policy sandbox.Policy, spec Spec) (preparedSpec, error) {
			return buildBubblewrapSpec(path, capabilities, policy, spec)
		},
	}
}

func bubblewrapProbeArgs(denyNetwork bool) []string {
	args := []string{
		"--die-with-parent", "--new-session", "--unshare-pid", "--unshare-uts", "--unshare-ipc",
		"--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
	}
	if denyNetwork {
		args = append(args, "--unshare-net")
	}
	return append(args, "--", "/bin/true")
}

func buildBubblewrapSpec(path string, capabilities sandbox.Capabilities, policy sandbox.Policy, spec Spec) (preparedSpec, error) {
	if policy.WorkspaceAccess != sandbox.WorkspaceReadOnly && policy.WorkspaceAccess != sandbox.WorkspaceReadWrite {
		return preparedSpec{}, failAdapter(sandbox.FailurePolicyInvalid, "bubblewrap requires explicit read_only or read_write workspace access")
	}
	workspace, err := canonicalWorkspace(policy.Workspace)
	if err != nil {
		return preparedSpec{}, err
	}
	workingDir, err := canonicalWorkingDir(workspace, spec.Dir)
	if err != nil {
		return preparedSpec{}, err
	}
	denyNetwork := policy.RequiredCapabilities().NetworkIsolation
	if denyNetwork && !capabilities.NetworkIsolation {
		return preparedSpec{}, failAdapter(sandbox.FailureCapabilityUnavailable, "bubblewrap network namespace is unavailable")
	}
	args := []string{
		"--die-with-parent", "--new-session", "--unshare-pid", "--unshare-uts", "--unshare-ipc",
		"--ro-bind", "/", "/",
	}
	command := spec.Executable
	if pathWithin(command, "/tmp") {
		// The isolated /tmp mount would hide test binaries and other legitimate
		// executables created in the host temporary directory. Preserve only the
		// requested executable as a read-only bind before replacing /tmp. A
		// writable staging mount is needed because the root bind is read-only.
		const stagedCommand = "/run/corelay-bwrap-command"
		args = append(args, "--tmpfs", "/run")
		args = append(args, "--ro-bind", command, stagedCommand)
		command = stagedCommand
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	if denyNetwork {
		args = append(args, "--unshare-net")
	}
	if policy.WorkspaceAccess == sandbox.WorkspaceReadWrite {
		args = append(args, "--bind", workspace, workspace)
	} else {
		args = append(args, "--ro-bind", workspace, workspace)
	}
	args = append(args, "--chdir", workingDir, "--", command)
	args = append(args, spec.Args...)
	applied := sandbox.Capabilities{FilesystemIsolation: true, ProcessIsolation: true}
	if denyNetwork {
		applied.NetworkIsolation = true
	}
	return preparedSpec{Spec: Spec{
		Executable: path, Args: args, Dir: workspace, Environment: cloneEnvironmentWithTemp(spec.Environment, "/tmp"),
	}, Applied: applied}, nil
}

func pathWithin(path, root string) bool {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return false
	}
	base, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(base, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return relative != "."
}
