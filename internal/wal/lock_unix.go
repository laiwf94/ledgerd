//go:build unix

package wal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked means another process has the log open.
var ErrLocked = errors.New("log is in use by another process")

// lock takes an exclusive advisory lock on the open log file, so two
// processes can never append to the same log. The kernel releases it when
// the file is closed or the process dies, including on kill -9.
func lock(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("%w: %s", ErrLocked, f.Name())
		}
		return err
	}
	return nil
}

// HoldLock takes the lock of the log in dir without creating or changing
// anything, so a tool can read the log while no server is writing it and
// none can start. It returns ErrLocked if a process has the log open. A
// directory without a lock file has never been opened by a server that
// could still be running; HoldLock then holds nothing.
func HoldLock(dir string) (release func(), err error) {
	f, err := os.Open(filepath.Join(dir, lockName))
	if errors.Is(err, os.ErrNotExist) {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := lock(f); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}
