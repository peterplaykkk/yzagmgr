//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package api

import (
	"errors"
	"os"
)

type operationLockMode uint8

const (
	operationLockShared operationLockMode = iota + 1
	operationLockExclusive
)

func tryOperationFileLock(*os.File, operationLockMode) (bool, error) {
	return false, errors.New("当前平台不支持安全的跨进程共享/独占文件锁")
}

func unlockOperationFile(*os.File) error {
	return errors.New("当前平台不支持安全的跨进程共享/独占文件锁")
}
