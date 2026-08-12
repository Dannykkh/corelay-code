package acpbridge

import "testing"

func TestStateLockIsExclusiveAndReusable(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireStateLock(dir)
	if err != nil {
		t.Fatalf("AcquireStateLock(first) error = %v", err)
	}
	if second, err := AcquireStateLock(dir); err == nil {
		_ = second.Close()
		t.Fatal("second state lock unexpectedly succeeded")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("StateLock.Close() error = %v", err)
	}
	third, err := AcquireStateLock(dir)
	if err != nil {
		t.Fatalf("AcquireStateLock(after close) error = %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("third StateLock.Close() error = %v", err)
	}
}
