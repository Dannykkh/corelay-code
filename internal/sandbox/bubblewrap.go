package sandbox

import "fmt"

func newBubblewrapAdapter(dependencies AdapterDependencies) Runner {
	path, err := dependencies.Lookup("bwrap")
	if err != nil {
		return NewUnavailableRunner(fmt.Sprintf("bubblewrap is unavailable: %v", err))
	}
	if err := dependencies.Probe(path, bubblewrapProbeArgs(false)); err != nil {
		return NewUnavailableRunner(fmt.Sprintf("bubblewrap capability probe failed: %v", err))
	}
	networkIsolation := dependencies.Probe(path, bubblewrapProbeArgs(true)) == nil
	capabilities := Capabilities{
		FilesystemIsolation:  true,
		NetworkIsolation:     networkIsolation,
		ProcessIsolation:     true,
		ProcessTreeKill:      true,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
	return &wrappedSandboxRunner{
		name:         "bubblewrap",
		capabilities: capabilities,
		host:         dependencies.Host,
		build: func(policy Policy, command CommandSpec) (preparedAdapterCommand, error) {
			return buildBubblewrapCommand(path, capabilities, policy, command)
		},
	}
}

func bubblewrapProbeArgs(denyNetwork bool) []string {
	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-uts",
		"--unshare-ipc",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
	}
	if denyNetwork {
		args = append(args, "--unshare-net")
	}
	return append(args, "--", "/bin/true")
}

func buildBubblewrapCommand(path string, capabilities Capabilities, policy Policy, command CommandSpec) (preparedAdapterCommand, error) {
	if policy.WorkspaceAccess != WorkspaceReadOnly && policy.WorkspaceAccess != WorkspaceReadWrite {
		return preparedAdapterCommand{}, &RunnerError{Code: FailurePolicyInvalid, Detail: "bubblewrap requires explicit read_only or read_write workspace access"}
	}
	workspace, err := canonicalWorkspace(policy.Workspace)
	if err != nil {
		return preparedAdapterCommand{}, err
	}
	workingDir, err := canonicalWorkingDir(workspace, command.Dir)
	if err != nil {
		return preparedAdapterCommand{}, err
	}
	denyNetwork := policy.RequiredCapabilities().NetworkIsolation
	if denyNetwork && !capabilities.NetworkIsolation {
		return preparedAdapterCommand{}, &RunnerError{Code: FailureCapabilityUnavailable, Detail: "bubblewrap network namespace is unavailable"}
	}

	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-uts",
		"--unshare-ipc",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
	}
	if denyNetwork {
		args = append(args, "--unshare-net")
	}
	if policy.WorkspaceAccess == WorkspaceReadWrite {
		args = append(args, "--bind", workspace, workspace)
	} else {
		args = append(args, "--ro-bind", workspace, workspace)
	}
	args = append(args, "--chdir", workingDir, "--", command.Path)
	args = append(args, command.Args...)

	applied := Capabilities{
		FilesystemIsolation: true,
		ProcessIsolation:    true,
	}
	if denyNetwork {
		applied.NetworkIsolation = true
	}
	return preparedAdapterCommand{
		Command: CommandSpec{
			Path:             path,
			Args:             args,
			Dir:              workspace,
			Environment:      cloneEnvironmentWithTemp(command.Environment, "/tmp"),
			Stdin:            append([]byte(nil), command.Stdin...),
			Timeout:          command.Timeout,
			OutputLimitBytes: command.OutputLimitBytes,
		},
		AppliedIsolation: applied,
	}, nil
}
