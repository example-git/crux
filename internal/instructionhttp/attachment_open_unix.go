//go:build unix

package instructionhttp

import (
	"os"
	"syscall"
)

func openAttachment(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
