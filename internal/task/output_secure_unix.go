//go:build !windows

package task

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openSecureDir(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func createSecureFile(dir *os.File, _ string, name string, mode os.FileMode) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if err := requireRegularFile(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func openSecureFile(dir *os.File, _ string, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if err := requireRegularFile(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func removeSecureFile(dir *os.File, _ string, name string) error {
	return unix.Unlinkat(int(dir.Fd()), name, 0)
}

func replaceSecureFile(dir *os.File, _ string, oldName, newName string) error {
	if err := unix.Renameat(int(dir.Fd()), oldName, int(dir.Fd()), newName); err != nil {
		return err
	}
	return unix.Fsync(int(dir.Fd()))
}

func requireRegularFile(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("task output entry is not a regular file")
	}
	return nil
}
