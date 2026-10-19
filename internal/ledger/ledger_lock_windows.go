//go:build windows

package ledger

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	lockFileEx   = kernel32.NewProc("LockFileEx")
	unlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33)
)

func tryLockFile(f *os.File) error {
	// All holders lock byte zero, even though the lock file is empty. The
	// standard library's Overlapped has the Windows OVERLAPPED ABI layout.
	var overlapped syscall.Overlapped
	ok, _, err := lockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately,
		0, 1, 0, uintptr(unsafe.Pointer(&overlapped))) // #nosec G103 -- synchronous Win32 call with an ABI-compatible OVERLAPPED
	if ok != 0 {
		return nil
	}
	if errors.Is(err, errorLockViolation) {
		return errLockBusy
	}
	return fmt.Errorf("ledger: LockFileEx: %w", err)
}

func unlockFile(f *os.File) error {
	var overlapped syscall.Overlapped
	ok, _, err := unlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped))) // #nosec G103 -- synchronous Win32 call with an ABI-compatible OVERLAPPED
	if ok == 0 {
		return fmt.Errorf("ledger: UnlockFileEx: %w", err)
	}
	return nil
}
