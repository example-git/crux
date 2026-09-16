//go:build windows

package client

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceChannelDialsNamedPipe(t *testing.T) {
	address := fmt.Sprintf(`\\.\pipe\crux-channel-%d-%d`, os.Getpid(), time.Now().UnixNano())
	listener, err := winio.ListenPipe(address, &winio.PipeConfig{
		MessageMode:      true,
		InputBufferSize:  65536,
		OutputBufferSize: 65536,
	})
	require.NoError(t, err)
	testWorkspaceChannelTransport(t, listener, "npipe", address)
}
