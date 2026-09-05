package agent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/lock"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestBackgroundAgentManagerAdmissionHonorsCallerCancellation(t *testing.T) {
	directory := t.TempDir()
	admission, err := newBackgroundAgentAdmission(directory, 1)
	require.NoError(t, err)
	releaseGate, err := lock.File(t.Context(), filepath.Join(directory, "admission.lock"))
	require.NoError(t, err)
	defer releaseGate()

	manager, err := newBackgroundAgentManager("workspace", nil, nil, admission)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result := make(chan error, 1)
	go func() {
		_, reserveErr := manager.ReserveContext(ctx, "prompt", "task", "description", managedtask.Ownership{ParentSessionID: "parent"})
		result <- reserveErr
	}()

	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, manager.ActiveCount())
	case <-time.After(500 * time.Millisecond):
		t.Fatal("canceled admission remained blocked on the global gate")
	}
}
