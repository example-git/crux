//go:build unix

package connection

import (
	"os"
	"syscall"
)

func openClientAuthorization(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
