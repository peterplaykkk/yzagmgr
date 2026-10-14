//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package api

import "testing"

func assertNativeUnexpectedFileLockError(*testing.T) bool { return false }
