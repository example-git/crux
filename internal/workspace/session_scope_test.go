package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestSessionScopeRejectsRecreatedIDBeforeRPC(t *testing.T) {
	w := NewClientWorkspace(nil, proto.Workspace{ID: "new-id"})
	defer w.subCancel()
	ctx := ContextWithSessionWorkspace(t.Context(), "old-id")
	// A nil SDK is intentional: reaching any RPC after this mismatch panics.
	_, err := w.GetSession(ctx, "session")
	require.Error(t, err)
	_, err = w.ListSessions(ctx)
	require.Error(t, err)
	_, err = w.ListMessages(ctx, "session")
	require.Error(t, err)
	_, err = w.ListSessionHistory(ctx, "session")
	require.Error(t, err)
	_, err = w.FileTrackerListReadFiles(ctx, "session")
	require.Error(t, err)
	_, err = w.ListUserMessages(ctx, "session")
	require.Error(t, err)
	_, err = w.ListAllUserMessages(ctx)
	require.Error(t, err)
	require.Error(t, w.SetCurrentSession(ctx, "session"))
	require.Empty(t, w.lastSession)
}
func TestSessionScopeSubscriptionReportsExactIncarnation(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		t.Run(map[bool]string{false: "reattach", true: "recreate"}[recreate], func(t *testing.T) {
			t.Cleanup(SetSSEBackoffForTest(time.Millisecond, 5*time.Millisecond))
			server := &recoveryServer{liveID: "ws-1", nextID: "ws-2"}
			if recreate {
				server.liveID = ""
			}
			c := server.start(t)
			w := NewClientWorkspace(c, proto.Workspace{ID: "ws-1", Path: "/tmp/scoped-session"})
			recorder := &connectionRecorder{}
			done := make(chan struct{})
			go func() { w.runSubscription(recorder.send); close(done) }()
			defer func() {
				w.Shutdown()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("subscription did not join")
				}
			}()
			require.Eventually(t, func() bool {
				recorder.mu.Lock()
				defer recorder.mu.Unlock()
				for _, event := range recorder.events {
					if event.State == ConnectionRecovered {
						return true
					}
				}
				return false
			}, 3*time.Second, 5*time.Millisecond)
			recorder.mu.Lock()
			events := append([]ConnectionEvent(nil), recorder.events...)
			recorder.mu.Unlock()
			recovered := false
			for _, event := range events {
				require.Same(t, w, event.Source)
				require.NotEmpty(t, event.WorkspaceID)
				if event.State == ConnectionRecovered && !recovered {
					recovered = true
					require.Equal(t, w.AuthenticationWorkspaceID(), event.WorkspaceID)
					require.Equal(t, recreate, event.Recreated)
					if recreate {
						require.Equal(t, "ws-1", event.PreviousWorkspaceID)
					} else {
						require.Empty(t, event.PreviousWorkspaceID)
					}
				}
			}
		})
	}
}
func TestSessionScopeAcceptedAuthorityIsDetachedMetadata(t *testing.T) {
	w := NewClientWorkspace(nil, proto.Workspace{ID: "display", Authority: &config.RemoteAuthority{Mode: "client", Principal: "owner", Revision: 4, Accounts: []config.RemoteAccountIdentity{{ProviderID: "provider", AccountID: "selected"}}}})
	defer w.subCancel()
	view := w.AcceptedAuthority()
	view.Revision = 99
	view.Accounts[0].AccountID = "changed"
	actual := w.AcceptedAuthority()
	require.EqualValues(t, 4, actual.Revision)
	require.Equal(t, "selected", actual.Accounts[0].AccountID)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := w.GetSession(ctx, "session")
	require.ErrorIs(t, err, context.Canceled)
}
