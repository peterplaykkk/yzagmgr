//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package api

import (
	"errors"
	"os"
	"syscall"
)

type operationLockMode uint8

const (
	operationLockShared operationLockMode = iota + 1
	operationLockExclusive
)

func tryOperationFileLock(file *os.File, mode operationLockMode) (bool, error) {
	flags := syscall.LOCK_NB
	if mode == operationLockShared {
		flags |= syscall.LOCK_SH
	} else {
		flags |= syscall.LOCK_EX
	}
	err := syscall.Flock(int(file.Fd()), flags)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

func unlockOperationFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
