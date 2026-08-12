package acpbridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// StateLock gives the standalone ACP process exclusive ownership of its
// dedicated durable-session namespace. The regular server uses a different
// namespace, so its process-local SessionStore mutex cannot race ACP writes.
type StateLock struct {
	file *os.File
	once sync.Once
	err  error
}

func AcquireStateLock(dir string) (*StateLock, error) {
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create ACP state directory: %w", err)
	}
	path := filepath.Join(dir, ".instance.lock")
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("ACP state lock must not be a symbolic link")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, errors.New("inspect ACP state lock failed")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("open ACP state lock failed")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("ACP state lock is not a regular file")
	}
	if info.Size() == 0 {
		if _, err := file.WriteAt([]byte{0}, 0); err != nil {
			_ = file.Close()
			return nil, errors.New("initialize ACP state lock failed")
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return nil, errors.New("sync ACP state lock failed")
		}
	}
	if err := lockFile(file); err != nil {
		_ = file.Close()
		return nil, errors.New("another ACP process already owns this state directory")
	}
	return &StateLock{file: file}, nil
}

func (l *StateLock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.file == nil {
			return
		}
		unlockErr := unlockFile(l.file)
		closeErr := l.file.Close()
		l.file = nil
		l.err = errors.Join(unlockErr, closeErr)
	})
	return l.err
}
