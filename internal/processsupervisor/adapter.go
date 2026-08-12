package processsupervisor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

type ExecutableLookup func(name string) (string, error)
type CommandProbe func(path string, args []string) error
type TempDirFactory func(pattern string) (string, error)
type AdapterFactory func(AdapterDependencies) Runner

type AdapterDependencies struct {
	Lookup  ExecutableLookup
	Probe   CommandProbe
	TempDir TempDirFactory
	Host    Runner
}

type AutoRunnerOptions struct {
	Platform   string
	Lookup     ExecutableLookup
	Probe      CommandProbe
	TempDir    TempDirFactory
	Factories  map[string]AdapterFactory
	Unconfined Runner
}

// AutoRunner selects one truthful platform adapter at construction time.
// Disabled is the only policy routed to HostRunner; isolating policies never
// degrade to host execution when setup or capability probing fails.
type AutoRunner struct {
	secure     Runner
	unconfined Runner
}

func NewAutoRunner() *AutoRunner {
	return NewAutoRunnerWithOptions(AutoRunnerOptions{})
}

func NewAutoRunnerWithOptions(options AutoRunnerOptions) *AutoRunner {
	platform := options.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	lookup := options.Lookup
	if lookup == nil {
		lookup = exec.LookPath
	}
	probe := options.Probe
	if probe == nil {
		probe = defaultCommandProbe
	}
	tempDir := options.TempDir
	if tempDir == nil {
		tempDir = func(pattern string) (string, error) { return os.MkdirTemp("", pattern) }
	}
	unconfined := options.Unconfined
	if unconfined == nil {
		unconfined = NewHostRunner()
	}
	factories := map[string]AdapterFactory{
		"darwin":  newSeatbeltAdapter,
		"linux":   newBubblewrapAdapter,
		"windows": newWindowsJobAdapter,
	}
	for name, factory := range options.Factories {
		factories[name] = factory
	}
	dependencies := AdapterDependencies{Lookup: lookup, Probe: probe, TempDir: tempDir, Host: unconfined}
	factory := factories[platform]
	if factory == nil {
		return &AutoRunner{
			secure:     NewUnavailableRunner(fmt.Sprintf("no secure streaming adapter for platform %q", platform)),
			unconfined: unconfined,
		}
	}
	secure := factory(dependencies)
	if secure == nil {
		secure = NewUnavailableRunner(fmt.Sprintf("secure streaming adapter for platform %q returned nil", platform))
	}
	return &AutoRunner{secure: secure, unconfined: unconfined}
}

func (r *AutoRunner) Name() string { return "auto" }

func (r *AutoRunner) Capabilities() sandbox.Capabilities {
	if r == nil || r.secure == nil {
		return sandbox.Capabilities{}
	}
	return r.secure.Capabilities()
}

func (r *AutoRunner) Start(ctx context.Context, policy sandbox.Policy, spec Spec) (*Process, Report) {
	if r == nil {
		return nil, Report{Policy: policy, Failure: sandbox.FailureRunnerUnavailable, Detail: "auto runner is nil"}
	}
	if policy.Enforcement == sandbox.EnforcementDisabled {
		if r.unconfined == nil {
			return nil, Report{Runner: r.Name(), Policy: policy, Failure: sandbox.FailureRunnerUnavailable, Detail: "disabled host adapter is unavailable"}
		}
		return r.unconfined.Start(ctx, policy, spec)
	}
	if r.secure == nil {
		return nil, Report{Runner: r.Name(), Policy: policy, Failure: sandbox.FailureRunnerUnavailable, Detail: "secure streaming adapter is unavailable"}
	}
	return r.secure.Start(ctx, policy, spec)
}

func defaultCommandProbe(path string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, args...)
	command.Env = make([]string, 0)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}
