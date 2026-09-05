//go:build darwin

package trafficcapture

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSplitProcessCommand(t *testing.T) {
	command, err := splitProcessCommand(`/path/tool "value with spaces" 'single value' escaped\ value ""`)
	require.NoError(t, err)
	require.Equal(t, []string{
		"/path/tool",
		"value with spaces",
		"single value",
		"escaped value",
		"",
	}, command)

	_, err = splitProcessCommand(`tool "unterminated`)
	require.ErrorContains(t, err, "unterminated")
}
