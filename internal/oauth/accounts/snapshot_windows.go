//go:build windows

package accounts

import (
	"crypto/sha256"
	"fmt"
	"os"
	"unsafe"

	"github.com/example-git/crux/internal/fsext"
	"golang.org/x/sys/windows"
)

func openAccountSnapshotFile(path string) (*os.File, error) { return fsext.OpenSharedRead(path) }

func accountFileIdentity(file *os.File, _ os.FileInfo) ([sha256.Size]byte, error) {
	handle := windows.Handle(file.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return [sha256.Size]byte{}, err
	}
	// FILE_BASIC_INFO supplies ChangeTime in addition to creation/write times.
	// LastAccessTime is intentionally excluded so unchanged reads remain stable.
	var basic struct {
		CreationTime, LastAccessTime, LastWriteTime, ChangeTime int64
		FileAttributes                                          uint32
		_                                                       uint32
	}
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(fmt.Appendf(nil, "%d:%d:%d:%d:%d:%d:%d", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow, basic.CreationTime, basic.LastWriteTime, basic.ChangeTime, basic.FileAttributes)), nil
}
