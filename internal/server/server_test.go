package server

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseHostURLClassifiesTCPHosts(t *testing.T) {
	for _, host := range []string{"tcp://127.0.0.1:9000", "tcp://[::1]:9000", "tcp://localhost:9000"} {
		parsed, err := ParseHostURL(host)
		require.NoError(t, err)
		require.Equal(t, "tcp", parsed.Scheme)
		require.True(t, IsLoopbackHost(parsed))
	}
	for _, host := range []string{"tcp://0.0.0.0:9000", "tcp://192.0.2.10:9000", "tcp://example.com:9000"} {
		parsed, err := ParseHostURL(host)
		require.NoError(t, err)
		require.False(t, IsLoopbackHost(parsed))
	}
}
