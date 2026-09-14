//go:build !darwin && !linux

package trafficcapture

import (
	"fmt"
	"os"
)

func bindWorkerExecutable(*workerConfig, *os.File) error {
	return fmt.Errorf("traffic capture executable binding is supported on Linux and macOS")
}
