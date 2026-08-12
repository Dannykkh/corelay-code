package sandbox

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
)

const (
	// DefaultOutputLimitBytes bounds zero-value CommandSpec callers without
	// requiring every composition point to opt in explicitly.
	DefaultOutputLimitBytes int64 = 4 << 20
	// MaximumOutputLimitBytes prevents callers from turning captured process
	// output back into an effectively unbounded allocation.
	MaximumOutputLimitBytes int64 = 64 << 20
)

type outputStream uint8

const (
	outputStreamStdout outputStream = iota
	outputStreamStderr
)

// boundedOutputCapture applies one combined byte budget to stdout and stderr.
// Bytes are retained in the order each stream reaches Write; the sum of both
// retained buffers never exceeds the normalized command limit.
type boundedOutputCapture struct {
	mu        sync.Mutex
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	remaining int64
	truncated bool
	onLimit   func()
	limitOnce sync.Once
}

type boundedOutputWriter struct {
	capture *boundedOutputCapture
	stream  outputStream
}

func newBoundedOutputCapture(limit int64, onLimit func()) *boundedOutputCapture {
	if limit == 0 {
		limit = DefaultOutputLimitBytes
	}
	return &boundedOutputCapture{remaining: limit, onLimit: onLimit}
}

func (c *boundedOutputCapture) stdoutWriter() *boundedOutputWriter {
	return &boundedOutputWriter{capture: c, stream: outputStreamStdout}
}

func (c *boundedOutputCapture) stderrWriter() *boundedOutputWriter {
	return &boundedOutputWriter{capture: c, stream: outputStreamStderr}
}

func (w *boundedOutputWriter) Write(payload []byte) (int, error) {
	if len(payload) == 0 {
		return 0, nil
	}
	capture := w.capture
	capture.mu.Lock()
	keep := len(payload)
	if int64(keep) > capture.remaining {
		keep = int(capture.remaining)
	}
	if keep > 0 {
		if w.stream == outputStreamStdout {
			_, _ = capture.stdout.Write(payload[:keep])
		} else {
			_, _ = capture.stderr.Write(payload[:keep])
		}
		capture.remaining -= int64(keep)
	}
	exceeded := keep < len(payload)
	if exceeded {
		capture.truncated = true
	}
	capture.mu.Unlock()

	if exceeded {
		capture.limitOnce.Do(func() {
			if capture.onLimit != nil {
				capture.onLimit()
			}
		})
	}
	// Report the entire chunk consumed after retaining only the allowed prefix.
	// Returning a short write would turn the pipe copy into an unrelated I/O
	// error and could obscure the typed output-limit terminal reason.
	return len(payload), nil
}

func (c *boundedOutputCapture) snapshot() (stdout, stderr []byte, truncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.stdout.Bytes()...), append([]byte(nil), c.stderr.Bytes()...), c.truncated
}

type executionTerminalReason uint32

const (
	executionTerminalNone executionTerminalReason = iota
	executionTerminalOutputLimit
	executionTerminalDeadline
	executionTerminalCanceled
)

// executionTerminal linearizes competing output-limit, deadline, and caller
// cancellation signals. The first successfully claimed reason is immutable.
type executionTerminal struct {
	reason atomic.Uint32
}

func (t *executionTerminal) claim(reason executionTerminalReason) bool {
	if reason == executionTerminalNone {
		return false
	}
	return t.reason.CompareAndSwap(uint32(executionTerminalNone), uint32(reason))
}

func (t *executionTerminal) claimOutputLimit(parent context.Context) bool {
	if reason := terminalReasonFromContext(parent); reason != executionTerminalNone {
		t.claim(reason)
		return false
	}
	return t.claim(executionTerminalOutputLimit)
}

func (t *executionTerminal) claimContext(parent context.Context) bool {
	return t.claim(terminalReasonFromContext(parent))
}

func (t *executionTerminal) load() executionTerminalReason {
	return executionTerminalReason(t.reason.Load())
}

func terminalReasonFromContext(ctx context.Context) executionTerminalReason {
	if ctx == nil {
		return executionTerminalNone
	}
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return executionTerminalDeadline
	case context.Canceled:
		return executionTerminalCanceled
	default:
		return executionTerminalNone
	}
}
