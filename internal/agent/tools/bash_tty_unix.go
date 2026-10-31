//go:build !windows

package tools

import "os"

func openAgentTerminal() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}
