//go:build windows

package task

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func openSecureDir(path string) (*os.File, error) {
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
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("task output root is not a protected directory")
	}
	return os.NewFile(uintptr(handle), path), nil
}

func createSecureFile(_ *os.File, root string, name string, mode os.FileMode) (*os.File, error) {
	path := filepath.Join(root, name)
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if err := requireRegularFileWindows(handle); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func openSecureFile(_ *os.File, root string, name string) (*os.File, error) {
	path := filepath.Join(root, name)
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if err := requireRegularFileWindows(handle); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func removeSecureFile(_ *os.File, root string, name string) error {
	file, err := openSecureFile(nil, root, name)
	if err != nil {
		return err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return err
	}
	return os.Remove(path)
}

func replaceSecureFile(_ *os.File, root string, oldName, newName string) error {
	oldPath, err := windows.UTF16PtrFromString(filepath.Join(root, oldName))
	if err != nil {
		return err
	}
	newPath, err := windows.UTF16PtrFromString(filepath.Join(root, newName))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(oldPath, newPath, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func requireRegularFileWindows(handle windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || fileType != windows.FILE_TYPE_DISK {
		return fmt.Errorf("task output entry is not a regular file")
	}
	return nil
}
