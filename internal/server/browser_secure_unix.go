//go:build !windows

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func validateOpenedWorkspaceDirectory(directory *os.File, path, root string) error {
	openedInfo, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect opened directory %q: %w", path, err)
	}
	current, err := openWorkspaceDirectory(root, path)
	if err != nil {
		return fmt.Errorf("revalidate opened directory %q: %w", path, err)
	}
	defer current.Close()
	currentInfo, err := current.Stat()
	if err != nil {
		return fmt.Errorf("inspect current directory %q: %w", path, err)
	}
	if !os.SameFile(openedInfo, currentInfo) {
		return fmt.Errorf("opened directory %q changed during authorization", path)
	}
	return nil
}

func openWorkspaceDirectory(root, path string) (*os.File, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("directory %q is outside its workspace root", path)
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), root)
	if relative == "." {
		return directory, nil
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		nextFD, openErr := unix.Openat(int(directory.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			directory.Close()
			return nil, openErr
		}
		next := os.NewFile(uintptr(nextFD), filepath.Join(directory.Name(), component))
		if closeErr := directory.Close(); closeErr != nil {
			next.Close()
			return nil, closeErr
		}
		directory = next
	}
	return directory, nil
}
