package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type managedServerStartLock struct {
	file   *os.File
	unlock func() error
}

func acquireManagedServerStartLock(ctx context.Context, path string) (*managedServerStartLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create managed server state directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open managed server startup lock: %w", err)
	}
	for {
		unlock, acquired, lockErr := tryManagedServerFileLock(file)
		if lockErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("acquire managed server startup lock: %w", lockErr)
		}
		if acquired {
			return &managedServerStartLock{file: file, unlock: unlock}, nil
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
}

func (l *managedServerStartLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	var unlockErr error
	if l.unlock != nil {
		unlockErr = l.unlock()
	}
	closeErr := l.file.Close()
	return errors.Join(unlockErr, closeErr)
}
