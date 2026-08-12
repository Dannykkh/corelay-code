package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLoopRegistry_RegisterAndRelease(t *testing.T) {
	r := NewLoopRegistry(2)

	sid, ctx, release, err := r.Register(context.Background(), "/tmp/a")
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if sid == "" {
		t.Fatal("Register returned empty session id")
	}
	if ctx == nil {
		t.Fatal("Register returned nil context")
	}
	if r.Count() != 1 {
		t.Fatalf("Count = %d, want 1", r.Count())
	}

	// Session must be listed
	snaps := r.List()
	if len(snaps) != 1 || snaps[0].SessionID != sid {
		t.Fatalf("List = %+v, want one snapshot with sid %s", snaps, sid)
	}
	if snaps[0].WorkDir != "/tmp/a" {
		t.Errorf("WorkDir = %q, want /tmp/a", snaps[0].WorkDir)
	}

	// Release should remove from registry and cancel context.
	release()
	if r.Count() != 0 {
		t.Errorf("Count after release = %d, want 0", r.Count())
	}
	select {
	case <-ctx.Done():
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Error("context not cancelled after release")
	}

	// Release is idempotent.
	release()
	if r.Count() != 0 {
		t.Errorf("Count after double-release = %d, want 0", r.Count())
	}
}

func TestLoopRegistry_SemaphoreCap(t *testing.T) {
	r := NewLoopRegistry(2)

	_, _, rel1, err := r.Register(context.Background(), "/p/1")
	if err != nil {
		t.Fatalf("first Register failed: %v", err)
	}
	_, _, rel2, err := r.Register(context.Background(), "/p/2")
	if err != nil {
		t.Fatalf("second Register failed: %v", err)
	}

	// Third should be rejected.
	_, _, _, err = r.Register(context.Background(), "/p/3")
	if !errors.Is(err, ErrTooManyLoops) {
		t.Fatalf("third Register err = %v, want ErrTooManyLoops", err)
	}

	// Release one slot — a new Register should now succeed.
	rel1()
	_, _, rel3, err := r.Register(context.Background(), "/p/3")
	if err != nil {
		t.Fatalf("Register after release failed: %v", err)
	}

	rel2()
	rel3()
	if r.Count() != 0 {
		t.Errorf("Count after all releases = %d, want 0", r.Count())
	}
}

func TestLoopRegistry_CancelBySessionID(t *testing.T) {
	r := NewLoopRegistry(3)

	sid, ctx, release, err := r.Register(context.Background(), "/p")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	defer release()

	if ok := r.Cancel(sid); !ok {
		t.Fatal("Cancel returned false for live session")
	}
	select {
	case <-ctx.Done():
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("context not cancelled after Cancel")
	}

	// Second Cancel is harmless but should report the loop as gone once
	// release removed it. Call release first.
	release()
	if ok := r.Cancel(sid); ok {
		t.Error("Cancel on released session returned true, want false")
	}
}

func TestLoopRegistry_CancelUnknown(t *testing.T) {
	r := NewLoopRegistry(1)
	if ok := r.Cancel("nope"); ok {
		t.Error("Cancel of unknown session returned true")
	}
}

func TestLoopRegistry_ParentCancelPropagates(t *testing.T) {
	r := NewLoopRegistry(1)

	parent, cancelParent := context.WithCancel(context.Background())
	_, ctx, release, err := r.Register(parent, "/p")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	defer release()

	cancelParent()
	select {
	case <-ctx.Done():
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("child ctx not cancelled when parent cancelled")
	}
}

func TestLoopRegistry_ByWorkDir(t *testing.T) {
	r := NewLoopRegistry(5)

	_, _, rel1, _ := r.Register(context.Background(), "/proj/a")
	// Make the timestamps strictly distinct so ordering is stable.
	time.Sleep(2 * time.Millisecond)
	_, _, rel2, _ := r.Register(context.Background(), "/proj/b")
	time.Sleep(2 * time.Millisecond)
	sidA2, _, rel3, _ := r.Register(context.Background(), "/proj/a")
	defer rel1()
	defer rel2()
	defer rel3()

	a := r.ByWorkDir("/proj/a")
	if len(a) != 2 {
		t.Fatalf("ByWorkDir /proj/a len = %d, want 2", len(a))
	}
	// Second entry is the most recent (sidA2)
	if a[1].SessionID != sidA2 {
		t.Errorf("ByWorkDir ordering: a[1].SessionID = %s, want %s", a[1].SessionID, sidA2)
	}

	b := r.ByWorkDir("/proj/b")
	if len(b) != 1 {
		t.Fatalf("ByWorkDir /proj/b len = %d, want 1", len(b))
	}

	if r.ByWorkDir("") != nil {
		t.Error("ByWorkDir(\"\") should return nil")
	}
	if r.ByWorkDir("/not-there") != nil {
		t.Error("ByWorkDir on unknown dir should return nil")
	}
}

func TestLoopRegistry_ConcurrentRegisterRelease(t *testing.T) {
	// Race-sensitive smoke test. Run with -race.
	const workers = 20
	const iters = 50
	r := NewLoopRegistry(4)

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				_, _, release, err := r.Register(context.Background(), "/w")
				if errors.Is(err, ErrTooManyLoops) {
					continue
				}
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				// Hold the slot briefly so the cap actually matters.
				time.Sleep(time.Millisecond)
				release()
			}
		}()
	}
	wg.Wait()

	if r.Count() != 0 {
		t.Errorf("Count after workers done = %d, want 0", r.Count())
	}
}

func TestLoopRegistry_ShutdownDrainsLiveLoops(t *testing.T) {
	r := NewLoopRegistry(3)

	_, ctx1, rel1, _ := r.Register(context.Background(), "/p/1")
	_, ctx2, rel2, _ := r.Register(context.Background(), "/p/2")

	// Simulate the loop goroutines: they block on their ctx and call
	// release on exit. This mirrors how handleAgentLoop defers release.
	go func() {
		<-ctx1.Done()
		time.Sleep(5 * time.Millisecond) // pretend tear-down work
		rel1()
	}()
	go func() {
		<-ctx2.Done()
		time.Sleep(5 * time.Millisecond)
		rel2()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if ok := r.Shutdown(ctx); !ok {
		t.Fatal("Shutdown returned false, expected clean drain")
	}
	if r.Count() != 0 {
		t.Errorf("Count after shutdown = %d, want 0", r.Count())
	}
	if !r.IsShuttingDown() {
		t.Error("IsShuttingDown = false after Shutdown")
	}
}

func TestLoopRegistry_ShutdownRejectsNewRegister(t *testing.T) {
	r := NewLoopRegistry(3)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if ok := r.Shutdown(ctx); !ok {
		t.Fatal("Shutdown of empty registry should drain instantly")
	}

	_, _, _, err := r.Register(context.Background(), "/p")
	if !errors.Is(err, ErrShuttingDown) {
		t.Errorf("Register after shutdown err = %v, want ErrShuttingDown", err)
	}
}

func TestLoopRegistry_ShutdownTimesOut(t *testing.T) {
	r := NewLoopRegistry(2)

	// Hold the slot — never release.
	_, _, _, err := r.Register(context.Background(), "/p")
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if ok := r.Shutdown(ctx); ok {
		t.Error("Shutdown returned true but loop never released")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("Shutdown blocked too long (%v), expected ~50ms", elapsed)
	}
}

func TestLoopRegistry_NonPositiveCap(t *testing.T) {
	r := NewLoopRegistry(0)
	if r.MaxConcurrent() != 1 {
		t.Errorf("MaxConcurrent = %d, want 1", r.MaxConcurrent())
	}
	_, _, rel, err := r.Register(context.Background(), "/p")
	if err != nil {
		t.Fatalf("Register on zero-cap registry failed: %v", err)
	}
	rel()
}

func TestLoopRegistry_RegistrationBarrierSerializesRegister(t *testing.T) {
	r := NewLoopRegistry(1)
	entered := make(chan struct{})
	releaseBarrier := make(chan struct{})
	barrierDone := make(chan error, 1)

	go func() {
		barrierDone <- r.WithRegistrationBarrier(func(active int) error {
			if active != 0 {
				t.Errorf("barrier active = %d, want 0", active)
			}
			close(entered)
			<-releaseBarrier
			return nil
		})
	}()
	<-entered

	type registerResult struct {
		release func()
		err     error
	}
	registered := make(chan registerResult, 1)
	go func() {
		_, _, release, err := r.Register(context.Background(), "/p")
		registered <- registerResult{release: release, err: err}
	}()

	select {
	case result := <-registered:
		if result.release != nil {
			result.release()
		}
		t.Fatal("Register completed while registration barrier was held")
	case <-time.After(20 * time.Millisecond):
		// Expected: Register is waiting to publish its loop.
	}

	close(releaseBarrier)
	if err := <-barrierDone; err != nil {
		t.Fatalf("WithRegistrationBarrier = %v", err)
	}
	result := <-registered
	if result.err != nil {
		t.Fatalf("Register after barrier = %v", result.err)
	}
	result.release()
}

func TestLoopRegistry_RegistrationBarrierReportsAllActiveLoops(t *testing.T) {
	r := NewLoopRegistry(3)
	_, _, releaseA, err := r.Register(context.Background(), "/workspace/a")
	if err != nil {
		t.Fatalf("Register A = %v", err)
	}
	defer releaseA()
	_, _, releaseB, err := r.Register(context.Background(), "/workspace/b")
	if err != nil {
		t.Fatalf("Register B = %v", err)
	}
	defer releaseB()

	if err := r.WithRegistrationBarrier(func(active int) error {
		if active != 2 {
			t.Fatalf("barrier active = %d, want 2", active)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithRegistrationBarrier = %v", err)
	}
}

func TestLoopRegistry_RegistrationBarrierFailsClosedDuringShutdown(t *testing.T) {
	r := NewLoopRegistry(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if ok := r.Shutdown(ctx); !ok {
		t.Fatal("Shutdown of empty registry should drain instantly")
	}

	called := false
	err := r.WithRegistrationBarrier(func(int) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("WithRegistrationBarrier after shutdown = %v, want ErrShuttingDown", err)
	}
	if called {
		t.Fatal("registration barrier invoked callback during shutdown")
	}
	if err := r.WithRegistrationBarrier(nil); !errors.Is(err, ErrInvalidRegistryOperation) {
		t.Fatalf("WithRegistrationBarrier(nil) = %v, want ErrInvalidRegistryOperation", err)
	}
}
