//go:build windows

package tools

import (
	"fmt"
	"os"
)

func openAgentTerminal() (*os.File, error) {
	return nil, fmt.Errorf("interactive Bash TTY execution is not supported on Windows")
}
