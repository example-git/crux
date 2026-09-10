package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var authenticationPeekNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

type authenticationPipeReader struct {
	ctx    context.Context
	file   *os.File
	handle windows.Handle
}

func prepareAuthenticationInput(ctx context.Context, input io.Reader) (io.Reader, error) {
	file, ok := input.(*os.File)
	if !ok {
		return input, nil
	}
	handle := windows.Handle(file.Fd())
	kind, err := windows.GetFileType(handle)
	if err != nil {
		return nil, err
	}
	if kind != windows.FILE_TYPE_PIPE {
		return input, nil
	}
	return &authenticationPipeReader{ctx: ctx, file: file, handle: handle}, nil
}

func (r *authenticationPipeReader) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		var available uint32
		result, _, err := authenticationPeekNamedPipe.Call(uintptr(r.handle), 0, 0, 0, uintptr(unsafe.Pointer(&available)), 0)
		if result == 0 {
			if errors.Is(err, windows.ERROR_BROKEN_PIPE) {
				return 0, io.EOF
			}
			return 0, err
		}
		if available > 0 {
			return r.file.Read(data[:min(len(data), int(available))])
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return 0, r.ctx.Err()
		case <-timer.C:
		}
	}
}
