//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func tryManagedServerFileLock(file *os.File) (func() error, bool, error) {
	overlapped := &windows.Overlapped{}
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped,
	)
	if err == windows.ERROR_LOCK_VIOLATION {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return func() error {
		return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped)
	}, true, nil
}
