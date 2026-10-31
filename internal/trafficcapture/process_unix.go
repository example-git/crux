//go:build darwin || linux

package trafficcapture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

func configureCaptureProcess(command *exec.Cmd, terminal *os.File) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	size, err := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
	if err != nil || size.Row == 0 || size.Col == 0 {
		size = &unix.Winsize{Row: 24, Col: 120}
	}
	_ = unix.IoctlSetWinsize(int(terminal.Fd()), unix.TIOCSWINSZ, size)
}

func foregroundCaptureTerminal() error {
	signal.Ignore(syscall.SIGTTOU)
	defer signal.Reset(syscall.SIGTTOU)
	return unix.IoctlSetPointerInt(int(os.Stdin.Fd()), unix.TIOCSPGRP, syscall.Getpgrp())
}

func signalCaptureProcess(pid int, force bool) error {
	signalValue := syscall.SIGTERM
	if force {
		signalValue = syscall.SIGKILL
	}
	err := syscall.Kill(-pid, signalValue)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func RunCapturedProcess(arguments []string) error {
	if len(arguments) < 2 {
		return fmt.Errorf("capture process requires an executable and its argument list")
	}
	status := os.NewFile(3, "capture-process-status")
	defer status.Close()
	encoder := json.NewEncoder(status)
	command := &exec.Cmd{
		Path: arguments[0], Args: append([]string{}, arguments[1:]...),
		Env: os.Environ(), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
	}
	if command.Path == "/proc/self/fd/4" {
		executable := os.NewFile(4, "approved-capture-executable")
		defer executable.Close()
		command.ExtraFiles = []*os.File{executable}
		command.Path = "/proc/self/fd/3"
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		_ = encoder.Encode(processEvent{Error: err.Error()})
		return err
	}
	if err := encoder.Encode(processEvent{PID: command.Process.Pid}); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	finished := make(chan int, 1)
	go func() {
		_ = command.Wait()
		code := command.ProcessState.ExitCode()
		if state, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && state.Signaled() {
			code = 128 + int(state.Signal())
		}
		finished <- code
	}()
	exited := false
	for {
		select {
		case code := <-finished:
			exited = true
			if err := encoder.Encode(processEvent{PID: command.Process.Pid, ExitCode: &code}); err != nil {
				return err
			}
		case received := <-signals:
			if !exited {
				_ = command.Process.Signal(received)
			}
		}
	}
}
