package ledger

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileLockExclusiveAndUnlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	if err := tryLockFile(first); err != nil {
		t.Fatal(err)
	}
	if err := tryLockFile(second); !errors.Is(err, errLockBusy) {
		t.Fatalf("competing handle: want busy, got %v", err)
	}
	if err := unlockFile(first); err != nil {
		t.Fatal(err)
	}
	if err := tryLockFile(second); err != nil {
		t.Fatalf("lock after explicit unlock: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tryLockFile(first); err != nil {
		t.Fatalf("lock after holder close: %v", err)
	}
}

func TestFileLockErrorIsNotBusy(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tryLockFile(f); err == nil || errors.Is(err, errLockBusy) {
		t.Fatalf("closed handle: want a non-retryable error, got %v", err)
	}
}
