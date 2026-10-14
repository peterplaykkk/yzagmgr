//go:build windows

package api

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

type operationLockMode uint8

const (
	operationLockShared operationLockMode = iota + 1
	operationLockExclusive
	lockFileFailImmediately = 0x00000001
	lockFileExclusive       = 0x00000002
	errorLockViolation      = syscall.Errno(33)
)

var (
	operationLockFileEx   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	operationUnlockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

func tryOperationFileLock(file *os.File, mode operationLockMode) (bool, error) {
	flags := uintptr(lockFileFailImmediately)
	if mode == operationLockExclusive {
		flags |= lockFileExclusive
	}
	var overlapped syscall.Overlapped
	r1, _, callErr := operationLockFileEx.Call(
		file.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 != 0 {
		return true, nil
	}
	if errors.Is(callErr, errorLockViolation) {
		return false, nil
	}
	if callErr == syscall.Errno(0) {
		return false, syscall.EINVAL
	}
	return false, callErr
}

func unlockOperationFile(file *os.File) error {
	var overlapped syscall.Overlapped
	r1, _, callErr := operationUnlockFileEx.Call(
		file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 != 0 {
		return nil
	}
	if callErr == syscall.Errno(0) {
		return syscall.EINVAL
	}
	return callErr
}
