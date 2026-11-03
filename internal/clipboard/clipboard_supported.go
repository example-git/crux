//go:build (darwin || linux || windows || freebsd || openbsd || netbsd) && !ios && !android

package clipboard

import (
	"context"
	"errors"

	"golang.design/x/clipboard"
)

func initClipboard() error {
	return clipboard.Init()
}

func writeText(text string) {
	_, _ = clipboard.Write(context.Background(), clipboard.FmtText, []byte(text))
}

func read(f Format) ([]byte, error) {
	var format clipboard.Format
	switch f {
	case FormatText:
		format = clipboard.FmtText
	case FormatImage:
		format = clipboard.FmtImage
	default:
		return nil, ErrEmpty
	}
	data, err := clipboard.Read(context.Background(), format)
	if errors.Is(err, clipboard.ErrNoData) || err == nil && data == nil {
		return nil, ErrEmpty
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}
