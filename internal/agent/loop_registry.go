package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrTooManyLoops is returned by LoopRegistry.Register when the concurrent
// loop budget is already exhausted.
var ErrTooManyLoops = errors.New("too many concurrent agent loops")

// ErrShuttingDown is returned by LoopRegistry.Register after Shutdown has
// been called. Callers should surface this as a 503 to the client so the
// UI understands the server is going away (as opposed to "try again in 5
// seconds", which is what ErrTooManyLoops means).
var ErrShuttingDown = errors.New("loop registry is shutting down")

// ErrInvalidRegistryOperation is returned when a caller supplies an invalid
// operation to a LoopRegistry synchronization boundary.
var ErrInvalidRegistryOperation = errors.New("invalid loop registry operation")

// ActiveLoopSnapshot is a read-only view of a running loop, safe to marshal
// to clients. It intentionally omits the cancel func.
type ActiveLoopSnapshot struct {
	SessionID string    `json:"sessionId"`
	WorkDir   string    `json:"workDir"`
	StartedAt time.Time `json:"startedAt"`
}

// activeLoop is the internal record held by the registry.
type activeLoop struct {
	SessionID string
	WorkDir   string
	StartedAt time.Time
	cancel    context.CancelFunc
}

// LoopRegistry tracks the set of agent loops currently running on the server.
//
// It serves four purposes:
//  1. Per-project isolation — callers can look up loops by workDir so that
//     the UI can show which projects are busy.
//  2. External cancellation — a POST /api/agent/{sessionId}/cancel handler
//     can call Cancel(sessionID) to stop a loop even if the SSE connection
//     is still open.
//  3. Concurrency cap — a counting semaphore rejects new loops once
//     maxConcurrent are already running, so the server cannot be flooded.
//  4. Graceful shutdown — Shutdown cancels every live loop and blocks
//     until each one's release() has been called (bounded by the supplied
//     context), so the process can exit without leaving goroutines
//     half-way through provider streams.
//
// All methods are safe for concurrent use.
type LoopRegistry struct {
	maxConcurrent int
	sem           chan struct{}

	mu           sync.RWMutex
	loops        map[string]*activeLoop // keyed by sessionID
	shuttingDown bool
	wg           sync.WaitGroup // incremented on Register, decremented in release()
}

// NewLoopRegistry creates a registry with the given concurrency cap.
// A non-positive cap is treated as 1 — zero would deadlock every caller.
func NewLoopRegistry(maxConcurrent int) *LoopRegistry {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &LoopRegistry{
		maxConcurrent: maxConcurrent,
		sem:           make(chan struct{}, maxConcurrent),
		loops:         make(map[string]*activeLoop),
	}
}

// MaxConcurrent returns the configured cap.
func (r *LoopRegistry) MaxConcurrent() int {
	return r.maxConcurrent
}

// WithRegistrationBarrier runs fn while loop registration and release are
// paused. active is the exact number of registered loops for the full duration
// of fn. This is the composition boundary for process-wide configuration
// mutations that must not race with a loop starting or finishing.
//
// The callback must be bounded and must not call another LoopRegistry method.
// A shutting-down registry fails closed without invoking fn.
func (r *LoopRegistry) WithRegistrationBarrier(fn func(active int) error) error {
	if fn == nil {
		return ErrInvalidRegistryOperation
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shuttingDown {
		return ErrShuttingDown
	}
	return fn(len(r.loops))
}

// Register reserves a slot, generates a session id, and returns a cancellable
// child context plus a release func that the caller must invoke when the
// loop finishes.
//
// Register is non-blocking: when the semaphore is full it returns
// ErrTooManyLoops immediately. Callers typically translate that into a 429.
//
// The returned context is cancelled when either (a) the parent context is
// cancelled, or (b) Cancel(sessionID) is called externally.
func (r *LoopRegistry) Register(parent context.Context, workDir string) (sessionID string, ctx context.Context, release func(), err error) {
	// Shutdown check must happen before touching the semaphore — once
	// shutdown starts we must not hand out new slots even if the cap
	// would otherwise allow it.
	r.mu.RLock()
	if r.shuttingDown {
		r.mu.RUnlock()
		return "", nil, nil, ErrShuttingDown
	}
	r.mu.RUnlock()

	// Semaphore: non-blocking reservation.
	select {
	case r.sem <- struct{}{}:
	default:
		return "", nil, nil, ErrTooManyLoops
	}

	// Between the semaphore reservation and the map insert, Shutdown
	// could have fired. Re-check under the write lock; if shutdown won,
	// return the slot and error out. This keeps the invariant that no
	// loop is ever registered after Shutdown begins.
	r.mu.Lock()
	if r.shuttingDown {
		r.mu.Unlock()
		<-r.sem
		return "", nil, nil, ErrShuttingDown
	}

	sessionID = newSessionID()
	childCtx, cancel := context.WithCancel(parent)
	loop := &activeLoop{
		SessionID: sessionID,
		WorkDir:   workDir,
		StartedAt: time.Now(),
		cancel:    cancel,
	}
	r.loops[sessionID] = loop
	r.wg.Add(1)
	r.mu.Unlock()

	var once sync.Once
	release = func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.loops, sessionID)
			r.mu.Unlock()
			cancel() // idempotent; safe even if already cancelled
			<-r.sem
			r.wg.Done()
		})
	}

	return sessionID, childCtx, release, nil
}

// Shutdown rejects new Register calls, cancels every live loop, and blocks
// until each loop's release() has been called or ctx is cancelled. Returns
// true if every loop drained cleanly, false if ctx expired with work still
// in flight.
//
// Shutdown is safe to call concurrently with Register and Cancel, but
// calling it more than once is a no-op after the first — subsequent calls
// just re-wait on the same wait group.
func (r *LoopRegistry) Shutdown(ctx context.Context) bool {
	// Flip the flag and grab the current set of loops under the write
	// lock, then cancel them outside the lock so cancel-triggered
	// release() calls don't deadlock on the mutex.
	r.mu.Lock()
	r.shuttingDown = true
	live := make([]*activeLoop, 0, len(r.loops))
	for _, l := range r.loops {
		live = append(live, l)
	}
	r.mu.Unlock()

	for _, l := range live {
		l.cancel()
	}

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// IsShuttingDown reports whether Shutdown has been called. Useful for
// health checks that want to fail fast during drain.
func (r *LoopRegistry) IsShuttingDown() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.shuttingDown
}

// Cancel stops a running loop by session id. Returns true if a loop was
// found and cancelled, false if the session id is unknown (already finished
// or never registered).
//
// Cancel does NOT release the semaphore slot — the owning goroutine's
// deferred release() call still runs when the loop returns. Calling Cancel
// twice is harmless.
func (r *LoopRegistry) Cancel(sessionID string) bool {
	r.mu.RLock()
	loop, ok := r.loops[sessionID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	loop.cancel()
	return true
}

// List returns snapshots of all active loops, ordered by StartedAt ascending.
func (r *LoopRegistry) List() []ActiveLoopSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ActiveLoopSnapshot, 0, len(r.loops))
	for _, l := range r.loops {
		out = append(out, ActiveLoopSnapshot{
			SessionID: l.SessionID,
			WorkDir:   l.WorkDir,
			StartedAt: l.StartedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// ByWorkDir returns snapshots of loops currently running in the given
// working directory. An empty workDir matches nothing.
func (r *LoopRegistry) ByWorkDir(workDir string) []ActiveLoopSnapshot {
	if workDir == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []ActiveLoopSnapshot
	for _, l := range r.loops {
		if l.WorkDir == workDir {
			out = append(out, ActiveLoopSnapshot{
				SessionID: l.SessionID,
				WorkDir:   l.WorkDir,
				StartedAt: l.StartedAt,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// Count returns the number of currently-running loops.
func (r *LoopRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.loops)
}

// newSessionID returns a 16-hex-character opaque identifier. 8 random bytes
// is overkill for uniqueness across a single server's lifetime but keeps the
// IDs short in logs and URLs.
func newSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail in practice; fall back to a
		// time-based id so the server keeps running.
		return "s" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}
