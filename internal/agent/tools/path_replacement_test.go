package tools

import (
	"errors"
	"runtime"
	"syscall"
)

func retainedDirectoryRenameBlocked(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	return errors.Is(err, syscall.Errno(5)) || errors.Is(err, syscall.Errno(32))
}
