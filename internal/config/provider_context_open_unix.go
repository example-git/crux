//go:build unix

package config

import (
	"os"
	"syscall"
)

// A regular path may become a FIFO after Stat. Open without waiting for a FIFO
// writer, then let the caller reject non-regular descriptors using fstat.
func openProviderContextFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
