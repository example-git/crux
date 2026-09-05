package trafficcapture

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/example-git/crux/internal/config"
)

func CaptureDirectory() string {
	return filepath.Join(storageDirectory(), "captures")
}

func storageDirectory() string {
	return filepath.Join(config.GlobalWorkspaceDir(), "traffic-capture")
}

func runDirectory() string {
	return filepath.Join(storageDirectory(), "runs")
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create private traffic capture directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect private traffic capture directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("private traffic capture path %q is not a directory", path)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure private traffic capture directory: %w", err)
	}
	return nil
}
