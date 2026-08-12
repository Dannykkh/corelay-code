//go:build windows

package processsupervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
	"golang.org/x/sys/windows"
)

const (
	windowsJobTerminationCode  = 0xC000013A
	windowsJobTerminationGrace = time.Second
	windowsJobPollInterval     = time.Millisecond
	windowsJobInitialPIDCount  = 8
	windowsJobMaximumPIDCount  = 1 << 20
)

// WindowsJobRunner starts the payload suspended, assigns it to a kill-on-close
// Job Object, and only then resumes its primary thread. Job Objects provide
// process containment and limits, not filesystem or network isolation.
type WindowsJobRunner struct {
	lookup ExecutableLookup
}

func newWindowsJobAdapter(dependencies AdapterDependencies) Runner {
	if err := probeWindowsJobObject(); err != nil {
		return NewUnavailableRunner(fmt.Sprintf("Windows Job Object setup failed: %v", err))
	}
	return &WindowsJobRunner{lookup: dependencies.Lookup}
}

func (r *WindowsJobRunner) Name() string { return "windows-job-object" }

func (r *WindowsJobRunner) Capabilities() sandbox.Capabilities {
	return sandbox.Capabilities{
		ProcessIsolation: true, ProcessLimits: true, MemoryLimits: true, ProcessTreeKill: true,
		EnvironmentFiltering: true, Timeouts: true,
	}
}

func (r *WindowsJobRunner) Start(ctx context.Context, policy sandbox.Policy, input Spec) (*Process, Report) {
	report := Report{Runner: r.Name(), Policy: policy, Capabilities: r.Capabilities()}
	fail := func(code sandbox.FailureCode, detail string) (*Process, Report) {
		report.Failure = code
		report.Detail = detail
		return nil, report
	}
	if policy.Enforcement == sandbox.EnforcementDisabled {
		return fail(sandbox.FailurePolicyInvalid, "Windows Job Object adapter does not accept disabled enforcement")
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
		return fail(contextFailure(err), "process context ended before sandbox setup")
	}

	lookup := r.lookup
	if lookup == nil {
		lookup = func(name string) (string, error) { return filepath.Abs(name) }
	}
	executable, err := lookup(spec.Executable)
	if err != nil {
		return fail(sandbox.FailureStartFailed, "resolve process executable")
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fail(sandbox.FailureStartFailed, "resolve process executable")
	}
	applicationName, err := windows.UTF16FromString(executable)
	if err != nil {
		return fail(sandbox.FailureCommandInvalid, "encode process executable")
	}
	commandLine, err := windows.UTF16FromString(windows.ComposeCommandLine(append([]string{executable}, spec.Args...)))
	if err != nil {
		return fail(sandbox.FailureCommandInvalid, "encode process command line")
	}
	currentDirectory, err := windows.UTF16PtrFromString(workingDir)
	if err != nil {
		return fail(sandbox.FailureCommandInvalid, "encode process working directory")
	}
	environmentBlock := encodeWindowsEnvironment(environment)

	job, err := createWindowsJob(policy)
	if err != nil {
		return fail(sandbox.FailureRunnerUnavailable, "prepare Windows Job Object")
	}
	jobOwned := true
	defer func() {
		if jobOwned {
			_ = windows.CloseHandle(job)
		}
	}()
	pipes, err := newWindowsStreamingPipes()
	if err != nil {
		return fail(sandbox.FailureRunnerUnavailable, "prepare process pipes")
	}
	defer pipes.closeAll()

	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fail(sandbox.FailureRunnerUnavailable, "prepare inherited handle list")
	}
	defer attributes.Delete()
	childHandles := []windows.Handle{pipes.stdinReadHandle(), pipes.stdoutWriteHandle(), pipes.stderrWriteHandle()}
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&childHandles[0]), uintptr(len(childHandles))*unsafe.Sizeof(childHandles[0])); err != nil {
		return fail(sandbox.FailureRunnerUnavailable, "configure inherited handle list")
	}
	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESTDHANDLES,
			StdInput: childHandles[0], StdOutput: childHandles[1], StdErr: childHandles[2],
		},
		ProcThreadAttributeList: attributes.List(),
	}
	processInfo := windows.ProcessInformation{}
	err = windows.CreateProcess(
		&applicationName[0], &commandLine[0], nil, nil, true,
		windows.CREATE_SUSPENDED|windows.CREATE_NEW_PROCESS_GROUP|windows.CREATE_DEFAULT_ERROR_MODE|windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT,
		&environmentBlock[0], currentDirectory, &startup.StartupInfo, &processInfo,
	)
	runtime.KeepAlive(childHandles)
	if err != nil {
		return fail(sandbox.FailureStartFailed, "create suspended process")
	}
	processOwned := true
	threadOwned := true
	defer func() {
		if threadOwned {
			_ = windows.CloseHandle(processInfo.Thread)
		}
		if processOwned {
			_ = windows.CloseHandle(processInfo.Process)
		}
	}()
	pipes.closeChildEnds()
	if err := windows.AssignProcessToJobObject(job, processInfo.Process); err != nil {
		terminateSuspendedWindowsProcess(processInfo.Process)
		return fail(sandbox.FailureRunnerUnavailable, "assign suspended process to Windows Job Object")
	}
	if err := ctx.Err(); err != nil {
		_ = windows.TerminateJobObject(job, windowsJobTerminationCode)
		_, _ = windows.WaitForSingleObject(processInfo.Process, windows.INFINITE)
		return fail(contextFailure(err), "process context ended before payload start")
	}
	if _, err := windows.ResumeThread(processInfo.Thread); err != nil {
		_ = windows.TerminateJobObject(job, windowsJobTerminationCode)
		_, _ = windows.WaitForSingleObject(processInfo.Process, windows.INFINITE)
		return fail(sandbox.FailureRunnerUnavailable, "resume sandboxed process")
	}
	_ = windows.CloseHandle(processInfo.Thread)
	threadOwned = false

	controller := newWindowsJobController(job, processInfo.Process)
	jobOwned = false
	processOwned = false
	stdin, stdout, stderr := pipes.takeParentEnds()
	wait := func() error {
		waitResult, waitErr := windows.WaitForSingleObject(processInfo.Process, windows.INFINITE)
		if waitErr != nil || waitResult != windows.WAIT_OBJECT_0 {
			if waitErr == nil {
				waitErr = fmt.Errorf("unexpected process wait result %d", waitResult)
			}
			_ = controller.terminate(context.Background())
			return waitErr
		}
		var exitCode uint32
		exitErr := windows.GetExitCodeProcess(processInfo.Process, &exitCode)
		terminationErr := controller.terminate(context.Background())
		if exitErr != nil {
			return exitErr
		}
		if terminationErr != nil {
			return terminationErr
		}
		if exitCode != 0 {
			return fmt.Errorf("process exited with code %d", exitCode)
		}
		return nil
	}
	process := newManagedProcess(nil, int(processInfo.ProcessId), stdin, stdout, stderr, wait, controller.terminate, nil, []func(){controller.close})
	report.Started = true
	report.Applied = sandbox.Capabilities{
		ProcessIsolation: true, ProcessTreeKill: true, EnvironmentFiltering: true, Timeouts: true,
		ProcessLimits: policy.MaxProcesses > 0, MemoryLimits: policy.MemoryLimitBytes > 0,
	}
	watchProcessContext(ctx, process)
	return process, report
}

type windowsJobController struct {
	job       windows.Handle
	process   windows.Handle
	once      sync.Once
	done      chan struct{}
	err       error
	closeOnce sync.Once
}

func newWindowsJobController(job, process windows.Handle) *windowsJobController {
	return &windowsJobController{job: job, process: process, done: make(chan struct{})}
}

func (c *windowsJobController) terminate(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.once.Do(func() {
		go func() {
			defer close(c.done)
			deadline := time.Now().Add(windowsJobTerminationGrace)
			processes, snapshotErr := snapshotWindowsJobProcesses(c.job)
			if snapshotErr != nil {
				c.err = fmt.Errorf("snapshot Windows Job Object processes: %w", snapshotErr)
			}
			if err := windows.TerminateJobObject(c.job, windowsJobTerminationCode); err != nil {
				_ = windows.TerminateProcess(c.process, windowsJobTerminationCode)
				if c.err == nil {
					c.err = fmt.Errorf("terminate Windows Job Object: %w", err)
				}
			}
			additional, resnapshotErr := snapshotWindowsJobProcesses(c.job)
			if resnapshotErr != nil {
				if c.err == nil {
					c.err = fmt.Errorf("resnapshot Windows Job Object processes: %w", resnapshotErr)
				}
			} else {
				processes = mergeWindowsJobProcessHandles(processes, additional)
			}
			if err := waitForWindowsJobProcesses(processes, deadline); err != nil && c.err == nil {
				c.err = err
			}
			closeWindowsJobProcessHandles(processes)
			for {
				active, err := windowsJobActiveProcessCount(c.job)
				if err != nil {
					if c.err == nil {
						c.err = fmt.Errorf("verify Windows Job Object drain: %w", err)
					}
					return
				}
				if active == 0 {
					return
				}
				if time.Now().After(deadline) {
					if c.err == nil {
						c.err = fmt.Errorf("verify Windows Job Object drain: %d process(es) remained active", active)
					}
					return
				}
				time.Sleep(windowsJobPollInterval)
			}
		}()
	})
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *windowsJobController) close() {
	c.closeOnce.Do(func() {
		<-c.done
		_ = windows.CloseHandle(c.process)
		_ = windows.CloseHandle(c.job)
	})
}

func probeWindowsJobObject() error {
	job, err := createWindowsJob(sandbox.Policy{})
	if err != nil {
		return err
	}
	return windows.CloseHandle(job)
}

func createWindowsJob(policy sandbox.Policy) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	information := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	information.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if policy.MaxProcesses > 0 {
		information.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		information.BasicLimitInformation.ActiveProcessLimit = policy.MaxProcesses
	}
	if policy.MemoryLimitBytes > 0 {
		memoryLimit := uintptr(policy.MemoryLimitBytes)
		if uint64(memoryLimit) != policy.MemoryLimitBytes {
			_ = windows.CloseHandle(job)
			return 0, fmt.Errorf("memory limit exceeds platform pointer size")
		}
		information.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		information.ProcessMemoryLimit = memoryLimit
		information.JobMemoryLimit = memoryLimit
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

type windowsJobBasicAccountingInformation struct {
	totalUserTime, totalKernelTime, thisPeriodTotalUserTime, thisPeriodTotalKernelTime int64
	totalPageFaultCount, totalProcesses, activeProcesses, totalTerminatedProcesses     uint32
}

type windowsJobProcessIDListHeader struct {
	assigned uint32
	listed   uint32
}

type windowsJobProcessHandle struct {
	pid    uint32
	handle windows.Handle
}

func snapshotWindowsJobProcesses(job windows.Handle) ([]windowsJobProcessHandle, error) {
	processIDs, err := windowsJobProcessIDs(job)
	if err != nil {
		return nil, err
	}
	processes := make([]windowsJobProcessHandle, 0, len(processIDs))
	for _, processID := range processIDs {
		process, openErr := windows.OpenProcess(windows.SYNCHRONIZE, false, processID)
		if errors.Is(openErr, windows.ERROR_INVALID_PARAMETER) {
			continue
		}
		if openErr != nil {
			closeWindowsJobProcessHandles(processes)
			return nil, fmt.Errorf("open Job Object process %d: %w", processID, openErr)
		}
		processes = append(processes, windowsJobProcessHandle{pid: processID, handle: process})
	}
	return processes, nil
}

func windowsJobProcessIDs(job windows.Handle) ([]uint32, error) {
	capacity := uint32(windowsJobInitialPIDCount)
	for {
		if capacity > windowsJobMaximumPIDCount {
			return nil, fmt.Errorf("Windows Job Object process count exceeds safe limit")
		}
		headerSize := unsafe.Sizeof(windowsJobProcessIDListHeader{})
		bufferSize := headerSize + uintptr(capacity)*unsafe.Sizeof(uintptr(0))
		buffer := make([]byte, int(bufferSize))
		header := (*windowsJobProcessIDListHeader)(unsafe.Pointer(&buffer[0]))
		err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(header)), uint32(len(buffer)), nil)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return nil, err
		}
		if header.assigned > windowsJobMaximumPIDCount || header.listed > windowsJobMaximumPIDCount {
			return nil, fmt.Errorf("Windows Job Object process count exceeds safe limit")
		}
		if err != nil || header.listed < header.assigned {
			next := header.assigned
			if next <= capacity {
				next = capacity * 2
			}
			capacity = next
			continue
		}
		if header.listed > capacity {
			return nil, fmt.Errorf("Windows Job Object returned too many process IDs")
		}
		raw := unsafe.Slice((*uintptr)(unsafe.Add(unsafe.Pointer(header), headerSize)), int(header.listed))
		processIDs := make([]uint32, 0, header.listed)
		for _, processID := range raw {
			if processID == 0 || uint64(processID) > uint64(^uint32(0)) {
				return nil, fmt.Errorf("Windows Job Object returned invalid process ID")
			}
			processIDs = append(processIDs, uint32(processID))
		}
		return processIDs, nil
	}
}

func mergeWindowsJobProcessHandles(existing, additional []windowsJobProcessHandle) []windowsJobProcessHandle {
	known := make(map[uint32]struct{}, len(existing)+len(additional))
	for _, process := range existing {
		known[process.pid] = struct{}{}
	}
	for _, process := range additional {
		if _, duplicate := known[process.pid]; duplicate {
			_ = windows.CloseHandle(process.handle)
			continue
		}
		known[process.pid] = struct{}{}
		existing = append(existing, process)
	}
	return existing
}

func waitForWindowsJobProcesses(processes []windowsJobProcessHandle, deadline time.Time) error {
	for _, process := range processes {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("Windows Job Object termination grace elapsed before process %d exited", process.pid)
		}
		milliseconds := uint32((remaining + time.Millisecond - 1) / time.Millisecond)
		result, err := windows.WaitForSingleObject(process.handle, milliseconds)
		if err != nil {
			return fmt.Errorf("wait for Windows Job Object process %d: %w", process.pid, err)
		}
		if result != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("Windows Job Object process %d survived termination grace", process.pid)
		}
	}
	return nil
}

func closeWindowsJobProcessHandles(processes []windowsJobProcessHandle) {
	for _, process := range processes {
		_ = windows.CloseHandle(process.handle)
	}
}

func windowsJobActiveProcessCount(job windows.Handle) (uint32, error) {
	information := windowsJobBasicAccountingInformation{}
	err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)), nil)
	return information.activeProcesses, err
}

type windowsStreamingPipes struct {
	stdinRead, stdinWrite, stdoutRead, stdoutWrite, stderrRead, stderrWrite *os.File
}

func newWindowsStreamingPipes() (*windowsStreamingPipes, error) {
	pipes := &windowsStreamingPipes{}
	var err error
	if pipes.stdinRead, pipes.stdinWrite, err = os.Pipe(); err != nil {
		return nil, err
	}
	if pipes.stdoutRead, pipes.stdoutWrite, err = os.Pipe(); err != nil {
		pipes.closeAll()
		return nil, err
	}
	if pipes.stderrRead, pipes.stderrWrite, err = os.Pipe(); err != nil {
		pipes.closeAll()
		return nil, err
	}
	for _, handle := range []windows.Handle{pipes.stdinReadHandle(), pipes.stdoutWriteHandle(), pipes.stderrWriteHandle()} {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			pipes.closeAll()
			return nil, err
		}
	}
	return pipes, nil
}

func (p *windowsStreamingPipes) stdinReadHandle() windows.Handle {
	return windows.Handle(p.stdinRead.Fd())
}
func (p *windowsStreamingPipes) stdoutWriteHandle() windows.Handle {
	return windows.Handle(p.stdoutWrite.Fd())
}
func (p *windowsStreamingPipes) stderrWriteHandle() windows.Handle {
	return windows.Handle(p.stderrWrite.Fd())
}

func (p *windowsStreamingPipes) closeChildEnds() {
	closeWindowsFile(&p.stdinRead)
	closeWindowsFile(&p.stdoutWrite)
	closeWindowsFile(&p.stderrWrite)
}

func (p *windowsStreamingPipes) takeParentEnds() (*os.File, *os.File, *os.File) {
	stdin, stdout, stderr := p.stdinWrite, p.stdoutRead, p.stderrRead
	p.stdinWrite, p.stdoutRead, p.stderrRead = nil, nil, nil
	return stdin, stdout, stderr
}

func (p *windowsStreamingPipes) closeAll() {
	for _, file := range []**os.File{&p.stdinRead, &p.stdinWrite, &p.stdoutRead, &p.stdoutWrite, &p.stderrRead, &p.stderrWrite} {
		closeWindowsFile(file)
	}
}

func closeWindowsFile(file **os.File) {
	if *file != nil {
		_ = (*file).Close()
		*file = nil
	}
}

func encodeWindowsEnvironment(environment []string) []uint16 {
	block := strings.Join(environment, "\x00") + "\x00"
	return append(utf16.Encode([]rune(block)), 0)
}

func terminateSuspendedWindowsProcess(process windows.Handle) {
	_ = windows.TerminateProcess(process, windowsJobTerminationCode)
	_, _ = windows.WaitForSingleObject(process, windows.INFINITE)
}

func contextFailure(err error) sandbox.FailureCode {
	if errors.Is(err, context.DeadlineExceeded) {
		return sandbox.FailureTimedOut
	}
	return sandbox.FailureCanceled
}
