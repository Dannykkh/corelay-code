package processsupervisor

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

func newSeatbeltAdapter(dependencies AdapterDependencies) Runner {
	if dependencies.Host == nil || !dependencies.Host.Capabilities().ProcessTreeKill {
		return NewUnavailableRunner("Seatbelt streaming supervision requires process-tree termination")
	}
	path, err := dependencies.Lookup("sandbox-exec")
	if err != nil {
		return NewUnavailableRunner(fmt.Sprintf("sandbox-exec is unavailable: %v", err))
	}
	probeProfile := "(version 1)\n(deny default)\n(allow process*)\n(allow file-read*)"
	if err := dependencies.Probe(path, []string{"-p", probeProfile, "/usr/bin/true"}); err != nil {
		return NewUnavailableRunner(fmt.Sprintf("sandbox-exec capability probe failed: %v", err))
	}
	capabilities := sandbox.Capabilities{
		FilesystemIsolation: true, NetworkIsolation: true, ProcessTreeKill: true,
		EnvironmentFiltering: true, Timeouts: true,
	}
	return &wrappedRunner{
		name: "seatbelt", capabilities: capabilities, host: dependencies.Host,
		build: func(policy sandbox.Policy, spec Spec) (preparedSpec, error) {
			return buildSeatbeltSpec(path, dependencies.TempDir, policy, spec)
		},
	}
}

func buildSeatbeltSpec(path string, tempFactory TempDirFactory, policy sandbox.Policy, spec Spec) (preparedSpec, error) {
	if policy.WorkspaceAccess != sandbox.WorkspaceReadOnly && policy.WorkspaceAccess != sandbox.WorkspaceReadWrite {
		return preparedSpec{}, failAdapter(sandbox.FailurePolicyInvalid, "seatbelt requires explicit read_only or read_write workspace access")
	}
	workspace, err := canonicalWorkspace(policy.Workspace)
	if err != nil {
		return preparedSpec{}, err
	}
	workingDir, err := canonicalWorkingDir(workspace, spec.Dir)
	if err != nil {
		return preparedSpec{}, err
	}
	temporary, err := tempFactory("corelay-mcp-sandbox-")
	if err != nil {
		return preparedSpec{}, failAdapter(sandbox.FailureRunnerUnavailable, "create isolated temporary directory")
	}
	cleanup := removeTemporaryDir(temporary)
	temporary, err = canonicalTemporaryDir(temporary)
	if err != nil {
		cleanup()
		return preparedSpec{}, err
	}
	profile := buildSeatbeltProfile(workspace, temporary, policy)
	args := []string{"-p", profile, spec.Executable}
	args = append(args, spec.Args...)
	applied := sandbox.Capabilities{FilesystemIsolation: true}
	if policy.RequiredCapabilities().NetworkIsolation {
		applied.NetworkIsolation = true
	}
	return preparedSpec{
		Spec:    Spec{Executable: path, Args: args, Dir: workingDir, Environment: cloneEnvironmentWithTemp(spec.Environment, temporary)},
		Applied: applied, Cleanup: removeTemporaryDir(temporary),
	}, nil
}

func buildSeatbeltProfile(workspace, temporary string, policy sandbox.Policy) string {
	lines := []string{
		"(version 1)", "(deny default)", "(allow process*)", "(allow signal)", "(allow file-read*)",
		"(allow sysctl-read)", "(allow mach-lookup)", "(allow ipc-posix-shm)",
		"(allow file-write* (literal \"/dev/null\"))",
		"(allow file-write* (subpath " + strconv.Quote(temporary) + "))",
	}
	if policy.WorkspaceAccess == sandbox.WorkspaceReadWrite {
		lines = append(lines, "(allow file-write* (subpath "+strconv.Quote(workspace)+"))")
	}
	if !policy.RequiredCapabilities().NetworkIsolation {
		lines = append(lines, "(allow network*)")
	}
	return strings.Join(lines, "\n")
}
