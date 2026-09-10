//go:build !windows

package fsext

import (
	"errors"
	"os"
)

func ValidatePrivateFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("private file has invalid type or permissions")
	}
	return nil
}
