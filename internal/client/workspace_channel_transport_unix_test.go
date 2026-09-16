//go:build unix

package client

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkspaceChannelDialsUnixSocket(t *testing.T) {
	root, err := os.MkdirTemp("", "crx-channel-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	address := filepath.Join(root, "channel.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", address)
	require.NoError(t, err)
	testWorkspaceChannelTransport(t, listener, "unix", address)
}
