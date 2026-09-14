//go:build linux

package trafficcapture

import (
	"fmt"
	"os"
)

func bindWorkerExecutable(config *workerConfig, executable *os.File) error {
	config.Command = append([]string{}, config.Command...)
	config.Command[0] = fmt.Sprintf("/proc/self/fd/%d", executable.Fd())
	config.PassFDs = []int{int(executable.Fd())}
	return nil
}
