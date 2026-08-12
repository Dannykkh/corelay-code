package processsupervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const stopTimeout = 5 * time.Second

// Spec is an immutable long-lived process description. Start clones every
// collection before it crosses the runner boundary.
type Spec struct {
	Executable  string
	Args        []string
	Dir         string
	Environment sandbox.EnvironmentSpec
}

// Report is the immutable capability and policy snapshot captured at start.
// It intentionally excludes argv, environment values, and process output.
type Report struct {
	Runner       string               `json:"runner"`
	Policy       sandbox.Policy       `json:"policy"`
	Capabilities sandbox.Capabilities `json:"capabilities"`
	Applied      sandbox.Capabilities `json:"applied"`
	Started      bool                 `json:"started"`
	Failure      sandbox.FailureCode  `json:"failure,omitempty"`
	Detail       string               `json:"detail,omitempty"`
}

// Runner starts a supervised process with streaming stdio. Implementations
// must validate the immutable policy snapshot before starting anything.
type Runner interface {
	Name() string
	Capabilities() sandbox.Capabilities
	Start(context.Context, sandbox.Policy, Spec) (*Process, Report)
}

// Process owns one started child and its stdio. Wait is performed exactly once
// in the background so callers can observe exit without leaking zombies.
type Process struct {
	command *exec.Cmd
	pid     int
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	outputs []io.Closer
	wait    func() error
	stop    func(context.Context) error
	cleanup []func()

	done     chan struct{}
	mu       sync.RWMutex
	err      error
	stopOnce sync.Once
	stopErr  error
	ioOnce   sync.Once
}

func newProcess(command *exec.Cmd, stdin io.WriteCloser, stdout, stderr io.ReadCloser, outputs ...io.Closer) *Process {
	pid := 0
	if command != nil && command.Process != nil {
		pid = command.Process.Pid
	}
	return newManagedProcess(
		command,
		pid,
		stdin,
		stdout,
		stderr,
		command.Wait,
		func(context.Context) error { return terminateProcess(command) },
		outputs,
		nil,
	)
}

// newManagedProcess is the common lifecycle boundary for exec.Cmd-backed and
// platform-native processes. In particular, Windows must retain its primary
// thread and Job Object handles across AssignProcessToJobObject and
// ResumeThread, which os/exec cannot represent safely.
func newManagedProcess(
	command *exec.Cmd,
	pid int,
	stdin io.WriteCloser,
	stdout, stderr io.ReadCloser,
	wait func() error,
	stop func(context.Context) error,
	outputs []io.Closer,
	cleanup []func(),
) *Process {
	process := &Process{
		command: command,
		pid:     pid,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		outputs: append([]io.Closer(nil), outputs...),
		wait:    wait,
		stop:    stop,
		cleanup: append([]func(){}, cleanup...),
		done:    make(chan struct{}),
	}
	go func() {
		err := process.wait()
		for _, output := range process.outputs {
			_ = output.Close()
		}
		for _, cleanup := range process.cleanup {
			if cleanup != nil {
				cleanup()
			}
		}
		process.mu.Lock()
		process.err = err
		process.mu.Unlock()
		close(process.done)
	}()
	return process
}

func (p *Process) Command() *exec.Cmd    { return p.command }
func (p *Process) Stdin() io.WriteCloser { return p.stdin }
func (p *Process) Stdout() io.ReadCloser { return p.stdout }
func (p *Process) Stderr() io.ReadCloser { return p.stderr }
func (p *Process) Done() <-chan struct{} { return p.done }
func (p *Process) PID() int {
	if p == nil {
		return 0
	}
	return p.pid
}

// CloseIO releases the parent-side streaming handles. Callers should invoke it
// after their framing/drain loops finish; it is safe to call more than once.
func (p *Process) CloseIO() {
	if p == nil {
		return
	}
	p.ioOnce.Do(func() {
		for _, stream := range []io.Closer{p.stdin, p.stdout, p.stderr} {
			if stream != nil {
				_ = stream.Close()
			}
		}
	})
}

func (p *Process) Wait(ctx context.Context) error {
	if p == nil {
		return os.ErrInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.err
	}
}

// Stop closes stdin, terminates the entire process group where the platform
// adapter can truthfully do so, and waits for the background reap.
func (p *Process) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	select {
	case <-p.done:
		return p.Wait(context.Background())
	default:
	}

	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
	}
	p.stopOnce.Do(func() {
		if p.stop == nil {
			p.stopErr = os.ErrProcessDone
			return
		}
		p.stopErr = p.stop(ctx)
	})
	waitErr := p.Wait(ctx)
	if p.stopErr != nil && !errors.Is(p.stopErr, os.ErrProcessDone) {
		return p.stopErr
	}
	return waitErr
}

// HostRunner is the explicitly unconfined legacy adapter. It accepts only a
// Disabled policy and never advertises isolation it does not provide.
type HostRunner struct{}

func NewHostRunner() *HostRunner   { return &HostRunner{} }
func (r *HostRunner) Name() string { return "host-disabled" }
func (r *HostRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		ProcessTreeKill:      processTreeKillSupported(),
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
}

func (r *HostRunner) Start(ctx context.Context, policy sandbox.Policy, input Spec) (*Process, Report) {
	report := Report{Runner: r.Name(), Policy: policy, Capabilities: r.Capabilities()}
	fail := func(code sandbox.FailureCode, detail string) (*Process, Report) {
		report.Failure = code
		report.Detail = detail
		return nil, report
	}
	if policy.Enforcement != sandbox.EnforcementDisabled {
		return fail(sandbox.FailureCapabilityUnavailable, "host runner requires explicitly disabled enforcement")
	}
	if err := sandbox.ValidatePolicy(policy, r.Capabilities()); err != nil {
		return fail(sandboxFailure(err), err.Error())
	}
	spec := cloneSpec(input)
	if err := validateSpec(spec); err != nil {
		return fail(sandbox.FailureCommandInvalid, err.Error())
	}
	workingDir, err := canonicalDirectory(spec.Dir)
	if err != nil {
		return fail(sandbox.FailureCommandInvalid, err.Error())
	}
	environment, err := sandbox.BuildEnvironment(spec.Environment)
	if err != nil {
		return fail(sandbox.FailureCommandInvalid, err.Error())
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		code := sandbox.FailureCanceled
		if errors.Is(err, context.DeadlineExceeded) {
			code = sandbox.FailureTimedOut
		}
		return fail(code, err.Error())
	}

	command := exec.Command(spec.Executable, spec.Args...)
	command.Dir = workingDir
	command.Env = environment
	configureProcess(command)
	stdin, err := command.StdinPipe()
	if err != nil {
		return fail(sandbox.FailureStartFailed, "prepare stdin pipe")
	}
	stdout, stdoutWriter := io.Pipe()
	stderr, stderrWriter := io.Pipe()
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		return fail(sandbox.FailureStartFailed, "process failed to start")
	}

	report.Started = true
	report.Applied = sandbox.Capabilities{
		ProcessTreeKill:      r.Capabilities().ProcessTreeKill,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
	process := newProcess(command, stdin, stdout, stderr, stdoutWriter, stderrWriter)
	watchProcessContext(ctx, process)
	return process, report
}

func watchProcessContext(ctx context.Context, process *Process) {
	go func() {
		select {
		case <-ctx.Done():
			stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			_ = process.Stop(stopCtx)
			cancel()
		case <-process.Done():
		}
	}()
}

// UnavailableRunner is the default for isolating MCP execution until a
// streaming OS sandbox adapter is explicitly installed. Preferred never
// degrades to HostRunner.
type UnavailableRunner struct{ reason string }

func NewUnavailableRunner(reason string) *UnavailableRunner {
	return &UnavailableRunner{reason: strings.TrimSpace(reason)}
}
func (r *UnavailableRunner) Name() string                       { return "unavailable" }
func (r *UnavailableRunner) Capabilities() sandbox.Capabilities { return sandbox.Capabilities{} }
func (r *UnavailableRunner) Start(_ context.Context, policy sandbox.Policy, _ Spec) (*Process, Report) {
	detail := r.reason
	if detail == "" {
		detail = "no secure long-lived process adapter is configured"
	}
	return nil, Report{
		Runner:       r.Name(),
		Policy:       policy,
		Capabilities: r.Capabilities(),
		Failure:      sandbox.FailureRunnerUnavailable,
		Detail:       detail,
	}
}

func cloneSpec(spec Spec) Spec {
	copySpec := spec
	copySpec.Args = append([]string(nil), spec.Args...)
	copySpec.Environment.Inherit = append([]string(nil), spec.Environment.Inherit...)
	copySpec.Environment.Set = make(map[string]string, len(spec.Environment.Set))
	for name, value := range spec.Environment.Set {
		copySpec.Environment.Set[name] = value
	}
	return copySpec
}

func validateSpec(spec Spec) error {
	if strings.TrimSpace(spec.Executable) == "" {
		return fmt.Errorf("executable is empty")
	}
	if strings.IndexByte(spec.Executable, 0) >= 0 {
		return fmt.Errorf("executable contains NUL")
	}
	for _, argument := range spec.Args {
		if strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("argument contains NUL")
		}
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("working directory is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve working directory")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("canonicalize working directory")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("working directory is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func sandboxFailure(err error) sandbox.FailureCode {
	var policyError *sandbox.PolicyError
	if errors.As(err, &policyError) {
		return policyError.Code
	}
	return sandbox.FailurePolicyInvalid
}
