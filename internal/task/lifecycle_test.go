package task

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatusTerminal(t *testing.T) {
	t.Parallel()

	require.False(t, StatusPending.Terminal())
	require.False(t, StatusRunning.Terminal())
	require.True(t, StatusCompleted.Terminal())
	require.True(t, StatusFailed.Terminal())
	require.True(t, StatusKilled.Terminal())
	require.True(t, StatusLost.Terminal())
	require.False(t, Status("unknown").Terminal())
}
