//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package api

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func assertNativeUnexpectedFileLockError(t *testing.T) bool {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "closed-lock-file")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	locked, err := tryOperationFileLock(file, operationLockShared)
	if locked || !errors.Is(err, syscall.EBADF) {
		t.Fatalf("closed file descriptor lock outcome = locked %v, err %v; want EBADF", locked, err)
	}
	lockPath := file.Name()
	first, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if locked, err := tryOperationFileLock(first, operationLockExclusive); err != nil || !locked {
		t.Fatalf("initial exclusive flock = locked %v, err %v", locked, err)
	}
	if locked, err := tryOperationFileLock(second, operationLockExclusive); err != nil || locked {
		t.Fatalf("contended nonblocking flock = locked %v, err %v; want busy without error", locked, err)
	}
	if err := unlockOperationFile(first); err != nil {
		t.Fatal(err)
	}
	return true
}
