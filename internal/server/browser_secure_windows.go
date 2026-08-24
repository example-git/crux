//go:build windows

package server

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func validateOpenedWorkspaceDirectory(directory *os.File, path, root string) error {
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(directory.Fd()), &opened); err != nil {
		return fmt.Errorf("inspect opened directory %q: %w", path, err)
	}
	current, err := openWorkspaceDirectory(root, path)
	if err != nil {
		return fmt.Errorf("revalidate opened directory %q: %w", path, err)
	}
	defer current.Close()
	var currentInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(current.Fd()), &currentInfo); err != nil {
		return fmt.Errorf("inspect current directory %q: %w", path, err)
	}
	if opened.VolumeSerialNumber != currentInfo.VolumeSerialNumber || opened.FileIndexHigh != currentInfo.FileIndexHigh || opened.FileIndexLow != currentInfo.FileIndexLow {
		return fmt.Errorf("opened directory %q changed during authorization", path)
	}
	return nil
}

func openWorkspaceDirectory(_, path string) (*os.File, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("path %q is not a directory", path)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("path %q is a reparse point", path)
	}
	return os.NewFile(uintptr(handle), path), nil
}
