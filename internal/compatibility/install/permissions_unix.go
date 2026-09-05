//go:build !windows

package install

import (
	"fmt"
	"io/fs"
	"os"
)

func validatePrivateAccess(path string, info fs.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("private compatibility directory %q has permissions %04o; expected no group or other access", path, info.Mode().Perm())
	}
	return nil
}

func createPrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}
