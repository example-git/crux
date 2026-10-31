package trafficcapture

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func captureTerminalPath(master *os.File) (string, error) {
	var name [128]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0])))
	if errno != 0 {
		return "", errno
	}
	return unix.ByteSliceToString(name[:]), nil
}

func openCaptureTerminal() (*os.File, *os.File, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	master := os.NewFile(uintptr(fd), "capture-pty")
	for _, request := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := unix.IoctlSetInt(fd, request, 0); err != nil {
			_ = master.Close()
			return nil, nil, err
		}
	}
	var name [128]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0])))
	if errno != 0 {
		_ = master.Close()
		return nil, nil, errno
	}
	slaveFD, err := unix.Open(unix.ByteSliceToString(name[:]), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	return master, os.NewFile(uintptr(slaveFD), "capture-tty"), nil
}
