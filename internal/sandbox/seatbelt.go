package sandbox

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func newSeatbeltAdapter(dependencies AdapterDependencies) Runner {
	path, err := dependencies.Lookup("sandbox-exec")
	if err != nil {
		return NewUnavailableRunner(fmt.Sprintf("sandbox-exec is unavailable: %v", err))
	}
	probeProfile := "(version 1)\n(deny default)\n(allow process*)\n(allow file-read*)"
	if err := dependencies.Probe(path, []string{"-p", probeProfile, "/usr/bin/true"}); err != nil {
		return NewUnavailableRunner(fmt.Sprintf("sandbox-exec capability probe failed: %v", err))
	}
	capabilities := Capabilities{
		FilesystemIsolation:  true,
		NetworkIsolation:     true,
		ProcessTreeKill:      true,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
	return &wrappedSandboxRunner{
		name:         "seatbelt",
		capabilities: capabilities,
		host:         dependencies.Host,
		build: func(policy Policy, command CommandSpec) (preparedAdapterCommand, error) {
			return buildSeatbeltCommand(path, dependencies.TempDir, policy, command)
		},
	}
}

func buildSeatbeltCommand(path string, tempFactory TempDirFactory, policy Policy, command CommandSpec) (preparedAdapterCommand, error) {
	if policy.WorkspaceAccess != WorkspaceReadOnly && policy.WorkspaceAccess != WorkspaceReadWrite {
		return preparedAdapterCommand{}, &RunnerError{Code: FailurePolicyInvalid, Detail: "seatbelt requires explicit read_only or read_write workspace access"}
	}
	workspace, err := canonicalWorkspace(policy.Workspace)
	if err != nil {
		return preparedAdapterCommand{}, err
	}
	workingDir, err := canonicalWorkingDir(workspace, command.Dir)
	if err != nil {
		return preparedAdapterCommand{}, err
	}
	tempDir, err := tempFactory("corelay-sandbox-")
	if err != nil {
		return preparedAdapterCommand{}, &RunnerError{Code: FailureRunnerUnavailable, Detail: fmt.Sprintf("create isolated temp directory: %v", err)}
	}
	tempDir, err = canonicalTemporaryDir(tempDir)
	if err != nil {
		return preparedAdapterCommand{}, err
	}
	cleanup := func() error {
		return os.RemoveAll(tempDir)
	}
	profile := buildSeatbeltProfile(workspace, tempDir, policy)
	args := []string{"-p", profile, command.Path}
	args = append(args, command.Args...)
	applied := Capabilities{FilesystemIsolation: true}
	if policy.RequiredCapabilities().NetworkIsolation {
		applied.NetworkIsolation = true
	}
	return preparedAdapterCommand{
		Command: CommandSpec{
			Path:             path,
			Args:             args,
			Dir:              workingDir,
			Environment:      cloneEnvironmentWithTemp(command.Environment, tempDir),
			Stdin:            append([]byte(nil), command.Stdin...),
			Timeout:          command.Timeout,
			OutputLimitBytes: command.OutputLimitBytes,
		},
		AppliedIsolation: applied,
		Cleanup:          cleanup,
	}, nil
}

func buildSeatbeltProfile(workspace, tempDir string, policy Policy) string {
	lines := []string{
		"(version 1)",
		"(deny default)",
		"(allow process*)",
		"(allow signal)",
		"(allow file-read*)",
		"(allow sysctl-read)",
		"(allow mach-lookup)",
		"(allow ipc-posix-shm)",
		"(allow file-write* (literal \"/dev/null\"))",
		"(allow file-write* (subpath " + strconv.Quote(tempDir) + "))",
	}
	if policy.WorkspaceAccess == WorkspaceReadWrite {
		lines = append(lines, "(allow file-write* (subpath "+strconv.Quote(workspace)+"))")
	}
	if !policy.RequiredCapabilities().NetworkIsolation {
		lines = append(lines, "(allow network*)")
	}
	return strings.Join(lines, "\n")
}
