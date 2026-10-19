//go:build unix

package ledger

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func tryLockFile(f *os.File) error {
	fd := int(f.Fd()) // #nosec G115 -- file descriptors fit in int
	err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR) {
		return errLockBusy
	}
	if err != nil {
		return fmt.Errorf("ledger: flock: %w", err)
	}
	return nil
}

func unlockFile(f *os.File) error {
	fd := int(f.Fd()) // #nosec G115 -- file descriptors fit in int
	return syscall.Flock(fd, syscall.LOCK_UN)
}
