package imagegen

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImagePlatformUsesCapturedTerminal(t *testing.T) {
	t.Setenv("TERM_PROGRAM", "not-captured")
	values, err := imagePlatformValues(t.Context(), []string{"TERM_PROGRAM=captured", "TERM_PROGRAM_VERSION=2", "TERM=xterm", "PRIVATE_TOKEN=secret"})
	require.NoError(t, err)
	require.Equal(t, runtime.GOOS, values["os"])
	require.Equal(t, runtime.GOARCH, values["arch"])
	require.Equal(t, "captured", values["terminal_program"])
	require.Equal(t, "2", values["terminal_version"])
	require.Equal(t, "xterm", values["term"])
	require.Len(t, values, 6)
	values, err = imagePlatformValues(t.Context(), nil)
	require.NoError(t, err)
	require.Empty(t, values["terminal_program"])
	for _, value := range []string{"bad\nheader", strings.Repeat("a", 257)} {
		_, err := imagePlatformValues(t.Context(), []string{"TERM=" + value})
		require.ErrorContains(t, err, "terminal identity")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = imagePlatformValues(ctx, nil)
	require.ErrorIs(t, err, context.Canceled)
	var output imagePlatformOutput
	_, err = output.Write(make([]byte, 1025))
	require.Error(t, err)
	require.Zero(t, output.Len())
}
