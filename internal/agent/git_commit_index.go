package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type gitCommitIndexLock struct {
	indexPath string
	lockPath  string
	file      *os.File
	published bool
}

type gitCommitIndexTransaction struct {
	indexPath       string
	snapshotPath    string
	originalBytes   []byte
	indexExisted    bool
	lock            *gitCommitIndexLock
	cleanupSnapshot func()
}

func beginGitCommitIndexTransaction(opts ToolExecutionOptions, workDir string) (*gitCommitIndexTransaction, error) {
	indexPath, err := gitCommitIndexPath(opts, workDir)
	if err != nil {
		return nil, err
	}
	lock, err := acquireGitCommitIndexLock(indexPath)
	if err != nil {
		return nil, err
	}
	transaction := &gitCommitIndexTransaction{indexPath: indexPath, lock: lock}
	data, err := os.ReadFile(indexPath)
	if errors.Is(err, os.ErrNotExist) {
		transaction.indexExisted = false
	} else if err != nil {
		_ = transaction.close()
		return nil, fmt.Errorf("read Git index: %w", err)
	} else {
		transaction.indexExisted = true
		transaction.originalBytes = data
	}

	snapshotPath, cleanup, err := createGitCommitIndexSnapshot(indexPath, transaction.originalBytes, transaction.indexExisted)
	if err != nil {
		_ = transaction.close()
		return nil, fmt.Errorf("create Git index snapshot: %w", err)
	}
	transaction.snapshotPath = snapshotPath
	transaction.cleanupSnapshot = cleanup
	return transaction, nil
}

func (t *gitCommitIndexTransaction) close() error {
	var firstErr error
	if t.lock != nil {
		if err := t.lock.close(); err != nil {
			firstErr = err
		}
		t.lock = nil
	}
	if t.cleanupSnapshot != nil {
		t.cleanupSnapshot()
		t.cleanupSnapshot = nil
	}
	return firstErr
}

func (t *gitCommitIndexTransaction) releaseLock() error {
	if t.lock == nil {
		return nil
	}
	err := t.lock.close()
	t.lock = nil
	return err
}

func (t *gitCommitIndexTransaction) publishSnapshot() error {
	if t.lock == nil {
		return errors.New("Git index transaction lock is not held")
	}
	data, err := os.ReadFile(t.snapshotPath)
	if err != nil {
		return fmt.Errorf("read staged Git index snapshot: %w", err)
	}
	if err := t.lock.publish(data); err != nil {
		return fmt.Errorf("publish staged Git index snapshot: %w", err)
	}
	t.lock = nil
	return nil
}

func gitCommitIndexPath(opts ToolExecutionOptions, workDir string) (string, error) {
	result := runToolProcess(opts, "GitCommit index path", workDir, "git", []string{"rev-parse", "--git-path", "index"}, defaultToolProcessTimeout)
	if err := gitCommitProcessError(result, "unable to locate the real index"); err != nil {
		return "", err
	}
	path := strings.TrimSpace(result.combinedOutput())
	if path == "" {
		return "", errors.New("git rev-parse returned an empty index path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	return filepath.Abs(path)
}

func acquireGitCommitIndexLock(indexPath string) (*gitCommitIndexLock, error) {
	lockPath := indexPath + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("index_busy: Git index is being updated; retry after the other Git operation finishes")
		}
		return nil, fmt.Errorf("acquire Git index lock: %w", err)
	}
	return &gitCommitIndexLock{indexPath: indexPath, lockPath: lockPath, file: file}, nil
}

func (l *gitCommitIndexLock) publish(data []byte) error {
	if l.file == nil {
		return errors.New("Git index lock is closed")
	}
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	if _, err := l.file.Seek(0, 0); err != nil {
		return err
	}
	if _, err := l.file.Write(data); err != nil {
		return err
	}
	if err := l.file.Sync(); err != nil {
		return err
	}
	if err := l.file.Close(); err != nil {
		l.file = nil
		return err
	}
	l.file = nil
	if err := replaceGitCommitIndexFile(l.lockPath, l.indexPath); err != nil {
		return err
	}
	l.published = true
	return nil
}

func (l *gitCommitIndexLock) close() error {
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		if err != nil {
			_ = os.Remove(l.lockPath)
			return err
		}
	}
	if !l.published {
		if err := os.Remove(l.lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func createGitCommitIndexSnapshot(indexPath string, data []byte, exists bool) (string, func(), error) {
	file, err := os.CreateTemp(filepath.Dir(indexPath), ".corelay-index-*.tmp")
	if err != nil {
		return "", nil, err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", nil, err
	}
	if exists {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			_ = os.Remove(path)
			return "", nil, err
		}
	} else if err := os.Remove(path); err != nil {
		return "", nil, err
	}
	cleanup := func() {
		_ = os.Remove(path)
		_ = os.Remove(path + ".lock")
	}
	return path, cleanup, nil
}
