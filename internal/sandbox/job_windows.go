//go:build windows

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsJobTerminationExitCode = 0xC000013A
	windowsJobTerminationGrace    = time.Second
	windowsJobDrainPollInterval   = time.Millisecond
	windowsJobInitialPIDCapacity  = 8
	windowsJobMaximumPIDCapacity  = 1 << 20
)

// WindowsJobRunner provides process-tree containment and optional active
// process/per-process memory limits. Job Objects do not isolate filesystem,
// network, or environment namespaces; those capabilities remain false.
type WindowsJobRunner struct {
	lookup               ExecutableLookup
	snapshotProcesses    windowsJobProcessSnapshotter
	activeProcessCount   windowsJobProcessCounter
	terminationGraceTime time.Duration
}

func newWindowsJobAdapter(dependencies AdapterDependencies) Runner {
	if err := probeWindowsJobObject(); err != nil {
		return NewUnavailableRunner(fmt.Sprintf("Windows Job Object setup failed: %v", err))
	}
	return &WindowsJobRunner{
		lookup:               dependencies.Lookup,
		snapshotProcesses:    snapshotWindowsJobProcesses,
		activeProcessCount:   windowsJobActiveProcessCount,
		terminationGraceTime: windowsJobTerminationGrace,
	}
}

func (r *WindowsJobRunner) Name() string {
	return "windows-job-object"
}

func (r *WindowsJobRunner) Capabilities() Capabilities {
	return Capabilities{
		ProcessIsolation:     true,
		ProcessLimits:        true,
		MemoryLimits:         true,
		ProcessTreeKill:      true,
		EnvironmentFiltering: true,
		Timeouts:             true,
	}
}

func (r *WindowsJobRunner) Run(ctx context.Context, policy Policy, command CommandSpec) (Result, Report) {
	report := Report{
		Runner:               r.Name(),
		RequestedEnforcement: policy.Enforcement,
		Capabilities:         r.Capabilities(),
	}
	if policy.Enforcement == EnforcementDisabled {
		return adapterSetupFailure(report, FailurePolicyInvalid, "Windows Job Object adapter does not accept disabled enforcement")
	}
	if err := ValidatePolicy(policy, r.Capabilities()); err != nil {
		code, detail := policyFailure(err)
		return adapterSetupFailure(report, code, detail)
	}
	if err := validateCommand(command); err != nil {
		return adapterSetupFailure(report, FailureCommandInvalid, err.Error())
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runContext := ctx
	cancel := func() {}
	if command.Timeout > 0 {
		runContext, cancel = context.WithTimeout(ctx, command.Timeout)
	}
	defer cancel()
	executionContext, cancelExecution := context.WithCancel(runContext)
	defer cancelExecution()
	terminal := &executionTerminal{}
	capture := newBoundedOutputCapture(command.OutputLimitBytes, func() {
		terminal.claimOutputLimit(runContext)
		cancelExecution()
	})
	if err := runContext.Err(); err != nil {
		return windowsContextSetupFailure(report, err)
	}

	executable, err := r.lookup(command.Path)
	if err != nil {
		return adapterSetupFailure(report, FailureStartFailed, fmt.Sprintf("resolve command executable: %v", err))
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return adapterSetupFailure(report, FailureStartFailed, fmt.Sprintf("make command executable absolute: %v", err))
	}
	environment, err := BuildEnvironment(command.Environment)
	if err != nil {
		return adapterSetupFailure(report, FailureCommandInvalid, err.Error())
	}
	environmentBlock := encodeWindowsEnvironment(environment)

	applicationName, err := windows.UTF16FromString(executable)
	if err != nil {
		return adapterSetupFailure(report, FailureCommandInvalid, fmt.Sprintf("encode command path: %v", err))
	}
	arguments := append([]string{executable}, command.Args...)
	commandLine, err := windows.UTF16FromString(windows.ComposeCommandLine(arguments))
	if err != nil {
		return adapterSetupFailure(report, FailureCommandInvalid, fmt.Sprintf("encode command line: %v", err))
	}
	var currentDirectory *uint16
	if command.Dir != "" {
		absoluteDirectory, absoluteErr := filepath.Abs(command.Dir)
		if absoluteErr != nil {
			return adapterSetupFailure(report, FailureCommandInvalid, fmt.Sprintf("make command directory absolute: %v", absoluteErr))
		}
		currentDirectory, err = windows.UTF16PtrFromString(absoluteDirectory)
		if err != nil {
			return adapterSetupFailure(report, FailureCommandInvalid, fmt.Sprintf("encode command directory: %v", err))
		}
	}

	job, err := createWindowsJob(policy)
	if err != nil {
		return adapterSetupFailure(report, FailureRunnerUnavailable, fmt.Sprintf("prepare Windows Job Object: %v", err))
	}
	jobOpen := true
	defer func() {
		if jobOpen {
			windows.CloseHandle(job)
		}
	}()

	pipes, err := newWindowsChildPipes()
	if err != nil {
		return adapterSetupFailure(report, FailureRunnerUnavailable, fmt.Sprintf("prepare command pipes: %v", err))
	}
	defer pipes.closeAll()
	attributeList, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return adapterSetupFailure(report, FailureRunnerUnavailable, fmt.Sprintf("prepare inherited handle list: %v", err))
	}
	defer attributeList.Delete()
	childHandles := []windows.Handle{pipes.stdinReadHandle(), pipes.stdoutWriteHandle(), pipes.stderrWriteHandle()}
	if err := attributeList.Update(
		windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&childHandles[0]),
		uintptr(len(childHandles))*unsafe.Sizeof(childHandles[0]),
	); err != nil {
		return adapterSetupFailure(report, FailureRunnerUnavailable, fmt.Sprintf("configure inherited handles: %v", err))
	}

	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:        uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags:     windows.STARTF_USESTDHANDLES,
			StdInput:  childHandles[0],
			StdOutput: childHandles[1],
			StdErr:    childHandles[2],
		},
		ProcThreadAttributeList: attributeList.List(),
	}
	processInfo := windows.ProcessInformation{}
	startedAt := time.Now()
	err = windows.CreateProcess(
		&applicationName[0],
		&commandLine[0],
		nil,
		nil,
		true,
		windows.CREATE_SUSPENDED|windows.CREATE_NEW_PROCESS_GROUP|windows.CREATE_DEFAULT_ERROR_MODE|windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT,
		&environmentBlock[0],
		currentDirectory,
		&startup.StartupInfo,
		&processInfo,
	)
	runtime.KeepAlive(childHandles)
	if err != nil {
		return adapterSetupFailure(report, FailureStartFailed, fmt.Sprintf("create suspended command: %v", err))
	}
	processOpen := true
	threadOpen := true
	defer func() {
		if threadOpen {
			windows.CloseHandle(processInfo.Thread)
		}
		if processOpen {
			windows.CloseHandle(processInfo.Process)
		}
	}()
	pipes.closeChildEnds()

	if err := windows.AssignProcessToJobObject(job, processInfo.Process); err != nil {
		terminateSuspendedWindowsProcess(processInfo.Process)
		return windowsPhysicalSetupFailure(report, startedAt, fmt.Sprintf("assign suspended command to Job Object: %v", err))
	}
	if err := runContext.Err(); err != nil {
		windows.TerminateJobObject(job, windowsJobTerminationExitCode)
		windows.WaitForSingleObject(processInfo.Process, windows.INFINITE)
		return windowsContextPhysicalSetupFailure(report, startedAt, err)
	}

	ioState := pipes.startIO(command.Stdin, capture)
	if _, err := windows.ResumeThread(processInfo.Thread); err != nil {
		windows.TerminateJobObject(job, windowsJobTerminationExitCode)
		windows.WaitForSingleObject(processInfo.Process, windows.INFINITE)
		windows.CloseHandle(job)
		jobOpen = false
		ioState.wait()
		return windowsPhysicalSetupFailure(report, startedAt, fmt.Sprintf("resume sandboxed command: %v", err))
	}
	windows.CloseHandle(processInfo.Thread)
	threadOpen = false

	waitResult := make(chan windowsProcessResult, 1)
	go func() {
		waitResult <- waitForWindowsProcess(processInfo.Process)
	}()
	var processResult windowsProcessResult
	var contextErr error
	var executionInterrupted bool
	var terminationErr error
	var terminationDeadline time.Time
	var terminationAttempted bool
	var terminationRequested bool
	var jobProcesses []windowsJobProcessHandle
	requestJobTermination := func() {
		if terminationAttempted {
			return
		}
		terminationAttempted = true
		terminationDeadline = time.Now().Add(r.windowsJobTerminationGrace())
		processes, snapshotErr := r.windowsJobProcessSnapshot(job)
		if snapshotErr != nil {
			recordWindowsJobTerminationError(&terminationErr, "snapshot Job Object processes", snapshotErr)
		} else {
			jobProcesses = mergeWindowsJobProcessHandles(jobProcesses, processes)
		}
		if terminateErr := windows.TerminateJobObject(job, windowsJobTerminationExitCode); terminateErr != nil {
			recordWindowsJobTerminationError(&terminationErr, "request Job Object termination", terminateErr)
			return
		}
		terminationRequested = true
	}
	select {
	case processResult = <-waitResult:
	case <-executionContext.Done():
		terminal.claimContext(runContext)
		if event, waitErr := windows.WaitForSingleObject(processInfo.Process, 0); waitErr == nil && event == windows.WAIT_OBJECT_0 {
			// The root won the race with cancellation; preserve its real exit.
			processResult = <-waitResult
		} else {
			executionInterrupted = true
			contextErr = runContext.Err()
			requestJobTermination()
			if !terminationRequested {
				// Ensure the root cannot keep running if Job Object termination
				// itself failed. Descendant containment is reported as failed
				// below rather than silently claiming process-tree kill.
				_ = windows.TerminateProcess(processInfo.Process, windowsJobTerminationExitCode)
			}
			processResult = <-waitResult
		}
	}
	terminal.claimContext(runContext)
	if !terminationAttempted {
		// A successful root exit does not imply its descendants exited. End
		// every remaining member before releasing our root-process reference.
		requestJobTermination()
	}
	if processes, snapshotErr := r.windowsJobProcessSnapshot(job); snapshotErr != nil {
		recordWindowsJobTerminationError(&terminationErr, "resnapshot terminating Job Object processes", snapshotErr)
	} else {
		jobProcesses = mergeWindowsJobProcessHandles(jobProcesses, processes)
	}
	windows.CloseHandle(processInfo.Process)
	processOpen = false
	if !terminationRequested {
		// KILL_ON_JOB_CLOSE is the last-resort kill request when the explicit
		// termination API failed. The run remains failed because closing the
		// handle gives us no Job Object state with which to verify containment.
		windows.CloseHandle(job)
		jobOpen = false
	}
	if waitErr := waitForWindowsJobProcesses(jobProcesses, terminationDeadline); waitErr != nil {
		recordWindowsJobTerminationError(&terminationErr, "wait for Job Object processes", waitErr)
	}
	closeWindowsJobProcessHandles(jobProcesses)
	jobProcesses = nil
	if terminationRequested {
		// TerminateJobObject is asynchronous. Exact member handles establish
		// process-object termination; ActiveProcesses reaching zero additionally
		// proves the Job Object accepted no remaining descendant association.
		if drainErr := waitForWindowsJobEmpty(job, terminationDeadline, r.windowsJobProcessCounter()); drainErr != nil {
			recordWindowsJobTerminationError(&terminationErr, "drain Job Object", drainErr)
		}
		windows.CloseHandle(job)
		jobOpen = false
	}
	ioState.wait()
	stdout, stderr, outputTruncated := capture.snapshot()
	terminalReason := terminal.load()
	if (terminalReason == executionTerminalDeadline || terminalReason == executionTerminalCanceled) && !executionInterrupted {
		// Preserve the root-exit-wins cancellation behavior while still treating
		// any observed output overflow as terminal.
		terminalReason = executionTerminalNone
	}
	if terminalReason == executionTerminalNone && outputTruncated {
		terminalReason = executionTerminalOutputLimit
	}

	result := Result{
		Started:         true,
		Stdout:          stdout,
		Stderr:          stderr,
		ExitCode:        int(processResult.exitCode),
		OutputTruncated: outputTruncated,
		Duration:        time.Since(startedAt),
	}
	report.Started = true
	report.EffectiveEnforcement = policy.Enforcement
	report.AppliedIsolation = Capabilities{
		ProcessIsolation: true,
		ProcessTreeKill:  terminationErr == nil,
		ProcessLimits:    policy.MaxProcesses > 0,
		MemoryLimits:     policy.MemoryLimitBytes > 0,
	}
	if terminalReason == executionTerminalDeadline && contextErr == context.DeadlineExceeded {
		result.TimedOut = true
	} else if terminalReason == executionTerminalCanceled && contextErr == context.Canceled {
		result.Canceled = true
	}
	switch {
	case terminationErr != nil:
		result.Err = &RunnerError{Code: FailureExecutionFailed, Detail: fmt.Sprintf("enforce Windows Job Object process-tree termination: %v", terminationErr)}
		report.Failure = FailureExecutionFailed
		report.Detail = result.Err.Error()
	case terminalReason == executionTerminalOutputLimit:
		result.Err = &RunnerError{Code: FailureOutputLimit, Detail: "command output exceeded its capture limit"}
		report.Failure = FailureOutputLimit
		report.Detail = result.Err.Error()
	case result.TimedOut:
		result.Err = &RunnerError{Code: FailureTimedOut, Detail: "command exceeded its execution deadline"}
		report.Failure = FailureTimedOut
		report.Detail = result.Err.Error()
	case result.Canceled:
		result.Err = &RunnerError{Code: FailureCanceled, Detail: "command execution was canceled"}
		report.Failure = FailureCanceled
		report.Detail = result.Err.Error()
	case processResult.err != nil:
		result.Err = processResult.err
		report.Failure = FailureExecutionFailed
		report.Detail = processResult.err.Error()
	case processResult.exitCode != 0:
		result.Err = &RunnerError{Code: FailureExecutionFailed, Detail: fmt.Sprintf("command exited with code %d", processResult.exitCode)}
		report.Failure = FailureExecutionFailed
		report.Detail = result.Err.Error()
	default:
		report.Failure = FailureNone
	}
	return result, report
}

// windowsJobBasicAccountingInformation mirrors
// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION. x/sys/windows exposes the query but
// not this information structure.
type windowsJobBasicAccountingInformation struct {
	totalUserTime             int64
	totalKernelTime           int64
	thisPeriodTotalUserTime   int64
	thisPeriodTotalKernelTime int64
	totalPageFaultCount       uint32
	totalProcesses            uint32
	activeProcesses           uint32
	totalTerminatedProcesses  uint32
}

type windowsJobProcessIDListHeader struct {
	numberOfAssignedProcesses uint32
	numberOfProcessIDsInList  uint32
}

type windowsJobProcessHandle struct {
	pid    uint32
	handle windows.Handle
}

type windowsJobProcessSnapshotter func(windows.Handle) ([]windowsJobProcessHandle, error)
type windowsJobProcessCounter func(windows.Handle) (uint32, error)

func (r *WindowsJobRunner) windowsJobProcessSnapshot(job windows.Handle) ([]windowsJobProcessHandle, error) {
	if r.snapshotProcesses != nil {
		return r.snapshotProcesses(job)
	}
	return snapshotWindowsJobProcesses(job)
}

func (r *WindowsJobRunner) windowsJobProcessCounter() windowsJobProcessCounter {
	if r.activeProcessCount != nil {
		return r.activeProcessCount
	}
	return windowsJobActiveProcessCount
}

func (r *WindowsJobRunner) windowsJobTerminationGrace() time.Duration {
	if r.terminationGraceTime > 0 {
		return r.terminationGraceTime
	}
	return windowsJobTerminationGrace
}

func waitForWindowsJobEmpty(job windows.Handle, deadline time.Time, activeProcessCount windowsJobProcessCounter) error {
	for {
		active, err := activeProcessCount(job)
		if err != nil {
			return fmt.Errorf("query active process count: %w", err)
		}
		if active == 0 {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("timed out waiting for %d active process(es) to terminate", active)
		}
		pause := windowsJobDrainPollInterval
		if remaining < pause {
			pause = remaining
		}
		time.Sleep(pause)
	}
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
			// The process exited between the Job Object query and OpenProcess.
			continue
		}
		if openErr != nil {
			closeWindowsJobProcessHandles(processes)
			return nil, fmt.Errorf("open Job Object process %d for synchronization: %w", processID, openErr)
		}
		processes = append(processes, windowsJobProcessHandle{pid: processID, handle: process})
	}
	return processes, nil
}

func windowsJobProcessIDs(job windows.Handle) ([]uint32, error) {
	capacity := uint32(windowsJobInitialPIDCapacity)
	for {
		if capacity > windowsJobMaximumPIDCapacity {
			return nil, fmt.Errorf("Job Object process count exceeds safe snapshot limit")
		}
		headerSize := unsafe.Sizeof(windowsJobProcessIDListHeader{})
		bufferSize := headerSize + uintptr(capacity)*unsafe.Sizeof(uintptr(0))
		buffer := make([]byte, int(bufferSize))
		header := (*windowsJobProcessIDListHeader)(unsafe.Pointer(&buffer[0]))
		queryErr := windows.QueryInformationJobObject(
			job,
			windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(header)),
			uint32(len(buffer)),
			nil,
		)
		if queryErr != nil && !errors.Is(queryErr, windows.ERROR_MORE_DATA) {
			return nil, queryErr
		}
		assigned := header.numberOfAssignedProcesses
		listed := header.numberOfProcessIDsInList
		if assigned > windowsJobMaximumPIDCapacity || listed > windowsJobMaximumPIDCapacity {
			return nil, fmt.Errorf("Job Object process count exceeds safe snapshot limit")
		}
		if queryErr != nil || listed < assigned {
			nextCapacity := assigned
			if nextCapacity <= capacity {
				nextCapacity = capacity * 2
			}
			capacity = nextCapacity
			continue
		}
		if listed > capacity {
			return nil, fmt.Errorf("Job Object returned %d process IDs into capacity %d", listed, capacity)
		}
		rawProcessIDs := unsafe.Slice(
			(*uintptr)(unsafe.Add(unsafe.Pointer(header), headerSize)),
			int(listed),
		)
		processIDs := make([]uint32, 0, listed)
		for _, rawProcessID := range rawProcessIDs {
			if rawProcessID == 0 || uint64(rawProcessID) > uint64(^uint32(0)) {
				return nil, fmt.Errorf("Job Object returned invalid process ID")
			}
			processIDs = append(processIDs, uint32(rawProcessID))
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
			return fmt.Errorf("termination grace elapsed before process %d exited", process.pid)
		}
		waitMilliseconds := uint32((remaining + time.Millisecond - 1) / time.Millisecond)
		wait, err := windows.WaitForSingleObject(process.handle, waitMilliseconds)
		if err != nil {
			return fmt.Errorf("wait for Job Object process %d: %w", process.pid, err)
		}
		if wait != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("termination grace elapsed while process %d remained active", process.pid)
		}
	}
	return nil
}

func closeWindowsJobProcessHandles(processes []windowsJobProcessHandle) {
	for _, process := range processes {
		_ = windows.CloseHandle(process.handle)
	}
}

func recordWindowsJobTerminationError(destination *error, stage string, err error) {
	if err != nil && *destination == nil {
		*destination = fmt.Errorf("%s: %w", stage, err)
	}
}

func windowsJobActiveProcessCount(job windows.Handle) (uint32, error) {
	information := windowsJobBasicAccountingInformation{}
	err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
		nil,
	)
	return information.activeProcesses, err
}

func probeWindowsJobObject() error {
	job, err := createWindowsJob(Policy{})
	if err != nil {
		return err
	}
	return windows.CloseHandle(job)
}

func createWindowsJob(policy Policy) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if policy.MaxProcesses > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		info.BasicLimitInformation.ActiveProcessLimit = policy.MaxProcesses
	}
	if policy.MemoryLimitBytes > 0 {
		memoryLimit := uintptr(policy.MemoryLimitBytes)
		if uint64(memoryLimit) != policy.MemoryLimitBytes {
			windows.CloseHandle(job)
			return 0, fmt.Errorf("memory limit exceeds platform pointer size")
		}
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.ProcessMemoryLimit = memoryLimit
		info.JobMemoryLimit = memoryLimit
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

type windowsChildPipes struct {
	stdinRead   *os.File
	stdinWrite  *os.File
	stdoutRead  *os.File
	stdoutWrite *os.File
	stderrRead  *os.File
	stderrWrite *os.File
}

func newWindowsChildPipes() (*windowsChildPipes, error) {
	pipes := &windowsChildPipes{}
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

func (p *windowsChildPipes) stdinReadHandle() windows.Handle {
	return windows.Handle(p.stdinRead.Fd())
}

func (p *windowsChildPipes) stdoutWriteHandle() windows.Handle {
	return windows.Handle(p.stdoutWrite.Fd())
}

func (p *windowsChildPipes) stderrWriteHandle() windows.Handle {
	return windows.Handle(p.stderrWrite.Fd())
}

func (p *windowsChildPipes) closeChildEnds() {
	closeFile(&p.stdinRead)
	closeFile(&p.stdoutWrite)
	closeFile(&p.stderrWrite)
}

func (p *windowsChildPipes) closeAll() {
	closeFile(&p.stdinRead)
	closeFile(&p.stdinWrite)
	closeFile(&p.stdoutRead)
	closeFile(&p.stdoutWrite)
	closeFile(&p.stderrRead)
	closeFile(&p.stderrWrite)
}

type windowsIOState struct {
	stdinWrite *os.File
	stdoutRead *os.File
	stderrRead *os.File
	capture    *boundedOutputCapture
	waitGroup  sync.WaitGroup
}

func (p *windowsChildPipes) startIO(stdin []byte, capture *boundedOutputCapture) *windowsIOState {
	state := &windowsIOState{
		stdinWrite: p.stdinWrite,
		stdoutRead: p.stdoutRead,
		stderrRead: p.stderrRead,
		capture:    capture,
	}
	p.stdinWrite = nil
	p.stdoutRead = nil
	p.stderrRead = nil
	state.waitGroup.Add(3)
	go func() {
		defer state.waitGroup.Done()
		if len(stdin) > 0 {
			_, _ = state.stdinWrite.Write(stdin)
		}
		_ = state.stdinWrite.Close()
	}()
	go func() {
		defer state.waitGroup.Done()
		_, _ = io.Copy(state.capture.stdoutWriter(), state.stdoutRead)
		_ = state.stdoutRead.Close()
	}()
	go func() {
		defer state.waitGroup.Done()
		_, _ = io.Copy(state.capture.stderrWriter(), state.stderrRead)
		_ = state.stderrRead.Close()
	}()
	return state
}

func (s *windowsIOState) wait() {
	s.waitGroup.Wait()
}

type windowsProcessResult struct {
	exitCode uint32
	err      error
}

func waitForWindowsProcess(process windows.Handle) windowsProcessResult {
	if _, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil {
		return windowsProcessResult{err: err}
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process, &exitCode); err != nil {
		return windowsProcessResult{err: err}
	}
	return windowsProcessResult{exitCode: exitCode}
}

func terminateSuspendedWindowsProcess(process windows.Handle) {
	_ = windows.TerminateProcess(process, windowsJobTerminationExitCode)
	_, _ = windows.WaitForSingleObject(process, windows.INFINITE)
}

func windowsContextSetupFailure(report Report, err error) (Result, Report) {
	if err == context.DeadlineExceeded {
		result, report := adapterSetupFailure(report, FailureTimedOut, "command deadline elapsed before sandbox setup")
		result.TimedOut = true
		return result, report
	}
	result, report := adapterSetupFailure(report, FailureCanceled, "command canceled before sandbox setup")
	result.Canceled = true
	return result, report
}

func windowsContextPhysicalSetupFailure(report Report, startedAt time.Time, err error) (Result, Report) {
	result, report := windowsContextSetupFailure(report, err)
	result.Duration = time.Since(startedAt)
	return result, report
}

func windowsPhysicalSetupFailure(report Report, startedAt time.Time, detail string) (Result, Report) {
	result, report := adapterSetupFailure(report, FailureRunnerUnavailable, detail)
	result.Duration = time.Since(startedAt)
	return result, report
}

func encodeWindowsEnvironment(environment []string) []uint16 {
	block := strings.Join(environment, "\x00") + "\x00"
	encoded := utf16.Encode([]rune(block))
	return append(encoded, 0)
}

func closeFile(file **os.File) {
	if *file != nil {
		_ = (*file).Close()
		*file = nil
	}
}
