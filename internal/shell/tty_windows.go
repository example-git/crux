//go:build windows

package shell

import (
	"errors"
	"fmt"
	"io"
	"os"

	"mvdan.cc/sh/v3/interp"
)

func prepareTTY(*os.File) (func(), error) {
	return nil, errors.New("interactive shell TTY execution is not supported on Windows")
}

func newTTYRunner(string, []string, *os.File, io.Writer, []BlockFunc) (*interp.Runner, error) {
	return nil, fmt.Errorf("interactive shell TTY execution is not supported on Windows")
}
