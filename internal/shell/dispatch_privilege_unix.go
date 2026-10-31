//go:build !windows

package shell

import (
	"io/fs"
	"syscall"
)

func trustedSetuidExecutable(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && info.Mode()&fs.ModeSetuid != 0 && info.Mode().Perm()&0o111 != 0 && stat.Uid == 0
}
