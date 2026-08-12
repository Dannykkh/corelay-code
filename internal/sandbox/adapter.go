package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"
)

type ExecutableLookup func(name string) (string, error)
type CommandProbe func(path string, args []string) error
type TempDirFactory func(pattern string) (string, error)
type AdapterFactory func(dependencies AdapterDependencies) Runner

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

// AutoRunner selects exactly one secure platform adapter when constructed.
// At execution time only an explicit Disabled policy is routed to Unconfined;
// Required and Preferred never degrade when the secure adapter is unavailable.
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
		tempDir = func(pattern string) (string, error) {
			return os.MkdirTemp("", pattern)
		}
	}
	unconfined := options.Unconfined
	if unconfined == nil {
		unconfined = NewUnconfinedRunner()
	}

	factories := map[string]AdapterFactory{
		"darwin":  newSeatbeltAdapter,
		"linux":   newBubblewrapAdapter,
		"windows": newWindowsJobAdapter,
	}
	for name, factory := range options.Factories {
		factories[name] = factory
	}
	dependencies := AdapterDependencies{
		Lookup:  lookup,
		Probe:   probe,
		TempDir: tempDir,
		Host:    unconfined,
	}
	factory, ok := factories[platform]
	if !ok || factory == nil {
		return &AutoRunner{
			secure:     NewUnavailableRunner(fmt.Sprintf("no sandbox adapter for platform %q", platform)),
			unconfined: unconfined,
		}
	}
	secure := factory(dependencies)
	if secure == nil {
		secure = NewUnavailableRunner(fmt.Sprintf("sandbox adapter for platform %q returned nil", platform))
	}
	return &AutoRunner{secure: secure, unconfined: unconfined}
}

func (r *AutoRunner) Name() string {
	return "auto"
}

func (r *AutoRunner) Capabilities() Capabilities {
	return r.secure.Capabilities()
}

func (r *AutoRunner) Run(ctx context.Context, policy Policy, command CommandSpec) (Result, Report) {
	if policy.Enforcement == EnforcementDisabled {
		return r.unconfined.Run(ctx, policy, command)
	}
	return r.secure.Run(ctx, policy, command)
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
