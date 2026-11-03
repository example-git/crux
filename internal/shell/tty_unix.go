//go:build !windows

package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/muesli/cancelreader"
	"golang.org/x/term"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

func newTTYRunner(cwd string, env []string, terminal *os.File, output io.Writer, blockFuncs []BlockFunc) (*interp.Runner, error) {
	options := []interp.RunnerOption{
		interp.StdIO(terminal, output, output),
		interp.Interactive(false),
		interp.Env(expand.ListEnviron(env...)),
		interp.Dir(cwd),
		execHandlerOptionWithBase(blockFuncs, ttyExecHandler(terminal)),
	}
	return interp.New(append(options, accessFilterOptions()...)...)
}

func prepareTTY(terminal *os.File) (func(), error) {
	if terminal == nil || !term.IsTerminal(int(terminal.Fd())) {
		return nil, fmt.Errorf("controlling terminal is not available")
	}
	state, err := term.MakeRaw(int(terminal.Fd()))
	if err != nil {
		return nil, fmt.Errorf("enable raw terminal mode: %w", err)
	}
	return func() {
		_ = term.Restore(int(terminal.Fd()), state)
	}, nil
}

func ttyExecHandler(terminal *os.File) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) (resultErr error) {
		hc := interp.HandlerCtx(ctx)
		path, err := interp.LookPathDir(hc.Dir, hc.Env, args[0])
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			return interp.ExitStatus(127)
		}

		rows, cols := uint16(24), uint16(80)
		if width, height, sizeErr := term.GetSize(int(terminal.Fd())); sizeErr == nil {
			cols = uint16(width)
			rows = uint16(height)
		}
		cmd := exec.Cmd{Path: path, Args: args, Env: execEnvList(hc.Env), Dir: hc.Dir}
		finishReporting, err := prepareNativeDirectoryReporting(ctx, &cmd)
		if err != nil {
			return err
		}
		defer func() {
			if err := finishReporting(); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		ptmx, err := pty.StartWithSize(&cmd, &pty.Winsize{Rows: rows, Cols: cols})
		if err != nil {
			return exitStatusFromError(ctx, hc.Stderr, err)
		}

		reader, err := cancelreader.NewReader(terminal)
		if err != nil {
			_ = ptmx.Close()
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("prepare terminal input: %w", err)
		}
		inputDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(ptmx, reader)
			close(inputDone)
		}()
		outputDone := make(chan struct{})
		go func() {
			_, _ = io.Copy(hc.Stdout, ptmx)
			close(outputDone)
		}()

		processDone := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
			timer := time.NewTimer(defaultKillTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			case <-processDone:
			}
		})
		err = cmd.Wait()
		close(processDone)
		stop()
		_ = reader.Cancel()
		_ = ptmx.Close()
		<-inputDone
		<-outputDone
		return exitStatusFromError(ctx, hc.Stderr, err)
	}
}
