package workspace

import (
	"context"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestProviderAuthenticationRecoveryPublicAdmission(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	request := ProviderAuthenticationRecoveryRequest(clientAuthenticationRecoveryAction(strings.Repeat("8", 32), f.target(t), 1))
	requests := f.requests.Load()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	outcome, err := f.w.RecoverProviderAuthentication(ctx, request)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, request.OperationID, outcome.OperationID)
	require.Equal(t, request.Target, outcome.Previous)
	require.Equal(t, requests, f.requests.Load())
	invalid := request
	invalid.RecoverySequence = 0
	_, err = f.w.RecoverProviderAuthentication(t.Context(), invalid)
	require.Error(t, err)
	require.Equal(t, requests, f.requests.Load())
	server := NewClientWorkspace(f.w.client, proto.Workspace{ID: request.Target.WorkspaceID})
	t.Cleanup(server.Shutdown)
	require.False(t, server.CanRecoverProviderAuthentication())
	_, err = server.RecoverProviderAuthentication(t.Context(), request)
	require.ErrorContains(t, err, "requires the owning client")
	require.Equal(t, requests, f.requests.Load(), "server-owned recovery must not send a client publication")
}
