package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Lock is an exclusive, cross-process lock on one state directory.
//
// Two daemons serving the same config would double-write the revision journal
// and could both claim the native takeover. The lock is therefore advisory at
// the OS level but mandatory in this product: every writer takes it, and a
// second one fails fast with ErrLocked instead of waiting.
type Lock struct {
	file *os.File
	path string
	// held reports whether the OS-level lock is actually held, so Release is
	// idempotent and safe after a failed Acquire.
	held bool
}

// LockName is the file inside the state directory.
const LockName = "daemon.lock"

// AcquireLock takes the state directory lock.
//
// macOS/Linux use flock(LOCK_EX|LOCK_NB), which the kernel releases when the
// process dies, so a crash cannot strand the directory. Windows uses
// LockFileEx with the same lifetime guarantee. Both are advisory: a process that
// ignores the file can still write, which is why the file also carries the
// owner pid for diagnostics.
func AcquireLock(dir string) (*Lock, error) {
	resolved, err := EnsurePrivateDir(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(resolved, LockName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(file); err != nil {
		file.Close()
		if isWouldBlock(err) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock_failed: %w", err)
	}
	// The owner pid is diagnostics only; it is never trusted for mutual
	// exclusion, because a pid can be reused after a crash.
	_ = file.Truncate(0)
	_, _ = file.WriteAt([]byte(fmt.Sprintf("%d\n%s\n", os.Getpid(), runtime.GOOS)), 0)
	return &Lock{file: file, path: path, held: true}, nil
}

// Path is the lock file, for diagnostics.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Held reports whether this handle currently owns the lock.
func (l *Lock) Held() bool { return l != nil && l.held }

// Release drops the lock. It is safe to call more than once and safe on a nil
// receiver, so a deferred call cannot panic on an error path.
func (l *Lock) Release() error {
	if l == nil || !l.held {
		return nil
	}
	l.held = false
	unlockErr := unlockFile(l.file)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// LockHeldByAnother reports whether some process currently holds the directory
// lock. It probes by attempting the lock itself and releasing it immediately,
// so it never guesses from a pid file.
func LockHeldByAnother(dir string) (bool, error) {
	lock, err := AcquireLock(dir)
	if err == ErrLocked {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, lock.Release()
}
