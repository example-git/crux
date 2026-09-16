package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

func TestSessionPresenceUsesPeerChannelAndReconnectsWithNewestIntent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("CRUX_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUX_CACHE_DIR", t.TempDir())
	t.Setenv("AI_CLI_DIR", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	serverCode, err := connection.EnsureServerIdentity(ctx)
	require.NoError(t, err)
	identity, err := connection.NewClientIdentity("session-presence-client")
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(ctx, "session-presence-client", identity.Certificate))
	serverTLS, err := connection.ServerTLSConfig(ctx)
	require.NoError(t, err)
	proxyTLS, err := connection.ClientTLSConfig(connection.Connection{ServerCertificate: serverCode, Client: identity})
	require.NoError(t, err)

	srv := server.NewServer(nil, "tcp", "127.0.0.1:0")
	require.NoError(t, srv.EnableNetworkAuth(ctx))
	var httpPresence atomic.Int32
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/current-session") {
			httpPresence.Add(1)
		}
		srv.Handler().ServeHTTP(writer, request)
	})
	backendServer := httptest.NewUnstartedServer(handler)
	backendServer.TLS = serverTLS
	backendServer.StartTLS()
	observed := make(chan proto.CurrentSession, 20)
	proxyServer := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/peer-channel") {
			proxyPeerChannel(t, writer, request, backendServer.URL, proxyTLS, func(fromClient bool, envelope proto.PeerEnvelope) peerChannelProxyDecision {
				if fromClient && envelope.Type == proto.PeerTypeSessionCurrentSet {
					var selection proto.CurrentSession
					require.NoError(t, json.Unmarshal(envelope.Payload, &selection))
					observed <- selection
				}
				return peerChannelProxyDecision{}
			})
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	proxyServer.TLS = serverTLS
	proxyServer.StartTLS()
	t.Cleanup(func() {
		proxyServer.Close()
		backendServer.Close()
		_ = srv.Close()
	})

	sdk, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(proxyServer.URL, "https://"), ServerCertificate: serverCode, Client: identity})
	require.NoError(t, err)
	oldWorkspace, err := sdk.CreateWorkspace(ctx, proto.Workspace{Path: t.TempDir(), AuthorityMode: "server"})
	require.NoError(t, err)
	newWorkspace, err := sdk.CreateWorkspace(ctx, proto.Workspace{Path: t.TempDir(), AuthorityMode: "server"})
	require.NoError(t, err)
	oldID, newID := oldWorkspace.ID, newWorkspace.ID
	first := NewClientWorkspace(sdk, *oldWorkspace)
	second := NewClientWorkspace(sdk, *oldWorkspace)
	t.Cleanup(first.Shutdown)
	t.Cleanup(second.Shutdown)

	require.NoError(t, first.SetCurrentSession(ctx, "A"))
	require.NoError(t, second.SetCurrentSession(ctx, "B"))
	selected, ok := sdk.CurrentSessionSelection(oldID, "")
	require.True(t, ok)
	require.Equal(t, "B", selected.SessionID)
	require.EqualValues(t, 2, selected.Generation)
	n, err := srv.Backend().AttachedClients(oldID, "B")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = srv.Backend().AttachedClients(oldID, "A")
	require.NoError(t, err)
	require.Zero(t, n)

	require.NoError(t, first.afterReconnect(func(tea.Msg) {}))
	current, _ := sdk.CurrentSessionSelection(oldID, "")
	require.Equal(t, selected, current)
	require.NoError(t, second.SetCurrentSession(ctx, ""))
	require.NoError(t, first.afterReconnect(func(tea.Msg) {}))
	cleared, _ := sdk.CurrentSessionSelection(oldID, "")
	require.Empty(t, cleared.SessionID)
	require.EqualValues(t, 3, cleared.Generation)

	first.mu.Lock()
	first.ws.ID = newID
	first.mu.Unlock()
	require.NoError(t, first.afterReconnect(func(tea.Msg) {}, oldID))
	recreated, ok := sdk.CurrentSessionSelection(newID, "")
	require.True(t, ok)
	require.Equal(t, cleared, recreated)
	require.NoError(t, first.SetCurrentSession(ctx, "new-receiver-selection"))
	require.NoError(t, first.afterReconnect(func(tea.Msg) {}, oldID))
	current, _ = sdk.CurrentSessionSelection(newID, "")
	require.Equal(t, "new-receiver-selection", current.SessionID)
	require.EqualValues(t, 4, current.Generation)
	n, err = srv.Backend().AttachedClients(newID, current.SessionID)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	var generation atomic.Uint64
	generation.Store(2)
	stale := ContextWithSessionSelection(ctx, newID, &generation, 1)
	require.ErrorContains(t, first.SetCurrentSession(stale, "stale-command"), "superseded")
	canceled, cancelSelection := context.WithCancel(ctx)
	cancelSelection()
	require.ErrorIs(t, first.SetCurrentSession(canceled, "canceled-command"), context.Canceled)
	require.Zero(t, httpPresence.Load())

	close(observed)
	var selections []proto.CurrentSession
	for selection := range observed {
		selections = append(selections, selection)
	}
	require.Len(t, selections, 8)
	for _, selection := range selections {
		require.NotNil(t, selection.SelectionGeneration)
	}
	require.Equal(t, "B", selections[2].SessionID)
	require.EqualValues(t, 2, *selections[2].SelectionGeneration)
	for _, index := range []int{3, 4, 5} {
		require.Empty(t, selections[index].SessionID)
		require.EqualValues(t, 3, *selections[index].SelectionGeneration)
	}
}
