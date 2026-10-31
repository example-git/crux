//go:build !darwin && !linux

package trafficcapture

import (
	"fmt"
	"os"
	"os/exec"
)

func openCaptureTerminal() (*os.File, *os.File, error) {
	return nil, nil, fmt.Errorf("capture process management requires macOS or Linux")
}

func configureCaptureProcess(command *exec.Cmd, terminal *os.File) {}

func foregroundCaptureTerminal() error {
	return fmt.Errorf("capture process management requires macOS or Linux")
}

func signalCaptureProcess(pid int, force bool) error {
	return fmt.Errorf("capture process management requires macOS or Linux")
}

func RunCapturedProcess(arguments []string) error {
	return fmt.Errorf("capture process management requires macOS or Linux")
}
