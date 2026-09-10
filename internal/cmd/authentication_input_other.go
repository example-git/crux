//go:build !windows

package cmd

import (
	"context"
	"io"
)

func prepareAuthenticationInput(_ context.Context, input io.Reader) (io.Reader, error) {
	return input, nil
}
