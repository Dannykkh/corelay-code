//go:build windows

package updater

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestInstallFailsClosedWhenWindowsTargetDeleteIsDenied models the share mode
// used by a running Windows executable. The updater must leave both the live
// target and the reserved rollback path untouched when the target cannot be
// renamed; it must never fall through to a destructive partial install.
func TestInstallFailsClosedWhenWindowsTargetDeleteIsDenied(t *testing.T) {
	root := t.TempDir()
	artifact := filepath.Join(root, "new.exe")
	target := filepath.Join(root, "current.exe")
	backup := filepath.Join(root, "current.previous")
	writeUpdaterFile(t, artifact, "new executable")
	writeUpdaterFile(t, target, "old executable")

	path, err := windows.UTF16PtrFromString(target)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(
		path,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("open target without delete sharing: %v", err)
	}
	defer windows.CloseHandle(handle)

	if _, err := Install(artifact, target, updaterDigest(t, artifact), backup); !errors.Is(err, ErrTargetInUse) {
		t.Fatalf("Install() error = %v, want ErrTargetInUse", err)
	}
	assertUpdaterFile(t, target, "old executable")
	if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup after refused install = %v, want absent", err)
	}
}
