//go:build darwin || linux

package trafficcapture

import (
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCaptureProcessCleansDescendantsAfterTargetExit(t *testing.T) {
	process := testCaptureProcess(t, "child")
	select {
	case <-process.targetDone:
	case <-time.After(5 * time.Second):
		t.Fatal("parent target did not exit")
	}
	require.Equal(t, "exited", process.snapshot().State)
	require.NoError(t, syscall.Kill(-process.command.Process.Pid, 0))
	require.NoError(t, process.stop(false))
	require.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(-process.command.Process.Pid, 0), syscall.ESRCH)
	}, 5*time.Second, 50*time.Millisecond)
}
