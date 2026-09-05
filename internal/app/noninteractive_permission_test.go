package app

import (
	"testing"
	"time"

	"github.com/example-git/crux/internal/permission"
	"github.com/stretchr/testify/require"
)

func TestNonInteractiveBypassDoesNotApproveReusedSession(t *testing.T) {
	permissions := permission.NewPermissionService(t.TempDir(), false, nil)
	request := permission.CreatePermissionRequest{
		SessionID:   "reused-session",
		ToolCallID:  "bypass-call",
		ToolName:    "bash",
		Action:      "execute",
		Description: "run command",
		Path:        t.TempDir(),
	}

	granted, err := permissions.Request(nonInteractivePermissionContext(t.Context(), true), request)
	require.NoError(t, err)
	require.True(t, granted)

	denyRequest := request
	denyRequest.ToolCallID = "deny-call"
	granted, err = permissions.Request(nonInteractivePermissionContext(t.Context(), false), denyRequest)
	require.False(t, granted)
	require.ErrorIs(t, err, permission.ErrInteractivePermissionUnavailable)

	events := permissions.Subscribe(t.Context())
	type result struct {
		granted bool
		err     error
	}
	interactiveResult := make(chan result, 1)
	go func() {
		interactiveRequest := request
		interactiveRequest.ToolCallID = "interactive-call"
		approved, requestErr := permissions.Request(t.Context(), interactiveRequest)
		interactiveResult <- result{granted: approved, err: requestErr}
	}()

	select {
	case event := <-events:
		require.Equal(t, "interactive-call", event.Payload.ToolCallID)
		require.True(t, permissions.Deny(event.Payload))
	case <-time.After(time.Second):
		t.Fatal("later interactive request was auto-approved by the bypass run")
	}
	interactive := <-interactiveResult
	require.NoError(t, interactive.err)
	require.False(t, interactive.granted)
}
