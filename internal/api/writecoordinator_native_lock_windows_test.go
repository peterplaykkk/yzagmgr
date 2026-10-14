//go:build windows

package api

import "testing"

func assertNativeUnexpectedFileLockError(*testing.T) bool { return false }
