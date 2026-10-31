package trafficcapture

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openCaptureTerminal() (*os.File, *os.File, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	master := os.NewFile(uintptr(fd), "capture-pty")
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	slaveFD, err := unix.Open(fmt.Sprintf("/dev/pts/%d", number), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	return master, os.NewFile(uintptr(slaveFD), "capture-tty"), nil
}
