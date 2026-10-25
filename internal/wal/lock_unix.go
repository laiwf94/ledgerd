//go:build unix

package wal

import (
	"errors"
	"fmt"
	"os"
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
