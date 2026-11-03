package shell

import (
	"context"
	"io"
	"io/fs"
	"os"

	"mvdan.cc/sh/v3/interp"
)

type AccessFilter interface {
	Command(context.Context, string, []string) error
	File(context.Context, string, string) error
}

type accessFilterKey struct{}
type redirectCaptureKey struct{}
type redirectWriterKey struct{}

func WithRedirectCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, redirectCaptureKey{}, true)
}

func bindRedirectCapture(ctx context.Context, stdout io.Writer) context.Context {
	if enabled, _ := ctx.Value(redirectCaptureKey{}).(bool); !enabled || ctx.Value(redirectWriterKey{}) != nil {
		return ctx
	}
	if stdout == nil {
		stdout = io.Discard
	}
	return context.WithValue(ctx, redirectWriterKey{}, stdout)
}

type capturedRedirect struct {
	io.ReadWriteCloser
	capture io.Writer
}

func (w *capturedRedirect) Write(data []byte) (int, error) {
	if n, err := w.capture.Write(data); err != nil {
		return n, err
	} else if n != len(data) {
		return n, io.ErrShortWrite
	}
	return w.ReadWriteCloser.Write(data)
}

func WithAccessFilter(ctx context.Context, filter AccessFilter) context.Context {
	return context.WithValue(ctx, accessFilterKey{}, filter)
}

func accessFilter(ctx context.Context) AccessFilter {
	filter, _ := ctx.Value(accessFilterKey{}).(AccessFilter)
	return filter
}

func accessFilterOptions() []interp.RunnerOption {
	return []interp.RunnerOption{
		interp.CallHandler(func(ctx context.Context, args []string) ([]string, error) {
			if filter := accessFilter(ctx); filter != nil {
				if err := filter.Command(ctx, interp.HandlerCtx(ctx).Dir, args); err != nil {
					return nil, err
				}
			}
			return args, nil
		}),
		interp.OpenHandler(func(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
			if filter := accessFilter(ctx); filter != nil {
				if err := filter.File(ctx, interp.HandlerCtx(ctx).Dir, path); err != nil {
					return nil, err
				}
			}
			file, err := interp.DefaultOpenHandler()(ctx, path, flag, perm)
			if err != nil {
				return nil, err
			}
			if capture, ok := ctx.Value(redirectWriterKey{}).(io.Writer); ok && flag&(os.O_WRONLY|os.O_RDWR) != 0 {
				return &capturedRedirect{ReadWriteCloser: file, capture: capture}, nil
			}
			return file, nil
		}),
		interp.ReadDirHandler2(func(ctx context.Context, path string) ([]fs.DirEntry, error) {
			if filter := accessFilter(ctx); filter != nil {
				if err := filter.File(ctx, interp.HandlerCtx(ctx).Dir, path); err != nil {
					return nil, err
				}
			}
			return interp.DefaultReadDirHandler2()(ctx, path)
		}),
		interp.StatHandler(func(ctx context.Context, path string, follow bool) (fs.FileInfo, error) {
			if filter := accessFilter(ctx); filter != nil {
				if err := filter.File(ctx, "", path); err != nil {
					return nil, err
				}
			}
			return interp.DefaultStatHandler()(ctx, path, follow)
		}),
	}
}

func OutputPaths(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	if source, ok := accessFilter(ctx).(interface{ OutputPaths() []string }); ok {
		return source.OutputPaths()
	}
	return nil
}
