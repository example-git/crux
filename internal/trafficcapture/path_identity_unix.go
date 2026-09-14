//go:build darwin || linux

package trafficcapture

import (
	"errors"
	"os"
	"syscall"
)

func identityFromFileInfo(info os.FileInfo) (pathIdentity, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return pathIdentity{}, errors.New("filesystem identity is unavailable")
	}
	return pathIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}
