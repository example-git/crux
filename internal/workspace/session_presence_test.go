package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/server"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// This fixture delays only the transport before dispatch. The registered
// production route, controller and backend perform every presence write.
func TestSessionPresenceDelayedHTTPAndReconnectUseNewestIntent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	srv := server.NewServer(nil, "unix", "")
	oldID, newID := uuid.NewString(), uuid.NewString()
	for _, id := range []string{oldID, newID} {
		backend.InsertWorkspaceForTest(srv.Backend(), &backend.Workspace{ID: id, Path: t.TempDir()})
	}
	entered, release := make(chan struct{}), make(chan struct{})
	releaseA := sync.OnceFunc(func() { close(release) })
	defer releaseA()
	var delay sync.Once
	observed := make(chan proto.CurrentSession, 20)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var selection proto.CurrentSession
		if err := json.Unmarshal(body, &selection); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		observed <- selection
		if selection.SessionID == "A" {
			delay.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		srv.Handler().ServeHTTP(w, r)
	}))
	defer func() { releaseA(); httpServer.Close() }()
	endpoint, err := url.Parse(httpServer.URL)
	require.NoError(t, err)
	sdk, err := client.NewClient(t.TempDir(), "tcp", endpoint.Host)
	require.NoError(t, err)
	for _, id := range []string{oldID, newID} {
		require.NoError(t, srv.Backend().AttachClient(id, sdk.ClientID()))
	}
	defer func() { _ = srv.Backend().RetireClient(sdk.ClientID()); srv.Backend().Shutdown() }()
	first := NewClientWorkspace(sdk, proto.Workspace{ID: oldID})
	second := NewClientWorkspace(sdk, proto.Workspace{ID: oldID})
	defer first.subCancel()
	defer second.subCancel()
	olderDone := make(chan error, 1)
	go func() { olderDone <- first.SetCurrentSession(ctx, "A") }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("older selection did not enter HTTP")
	}
	require.NoError(t, second.SetCurrentSession(ctx, "B"))
	selected, ok := sdk.CurrentSessionSelection(oldID, "")
	require.True(t, ok)
	require.Equal(t, "B", selected.SessionID)
	require.EqualValues(t, 2, selected.Generation)
	releaseA()
	select {
	case err := <-olderDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("older selection did not finish")
	}
	n, err := srv.Backend().AttachedClients(oldID, "B")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = srv.Backend().AttachedClients(oldID, "A")
	require.NoError(t, err)
	require.Zero(t, n)
	// The first wrapper must reassert B, not its own earlier A. Reconnect
	// preserves the exact generation instead of minting a new selection.
	first.afterReconnect(func(tea.Msg) {})
	current, _ := sdk.CurrentSessionSelection(oldID, "")
	require.Equal(t, selected, current)
	require.NoError(t, second.SetCurrentSession(ctx, ""))
	first.afterReconnect(func(tea.Msg) {})
	cleared, _ := sdk.CurrentSessionSelection(oldID, "")
	require.Empty(t, cleared.SessionID)
	require.EqualValues(t, 3, cleared.Generation)
	first.mu.Lock()
	first.ws.ID = newID
	first.mu.Unlock()
	first.afterReconnect(func(tea.Msg) {}, oldID)
	recreated, ok := sdk.CurrentSessionSelection(newID, "")
	require.True(t, ok)
	require.Equal(t, cleared, recreated)
	require.NoError(t, first.SetCurrentSession(ctx, "new-receiver-selection"))
	first.afterReconnect(func(tea.Msg) {}, oldID)
	current, _ = sdk.CurrentSessionSelection(newID, "")
	require.Equal(t, "new-receiver-selection", current.SessionID)
	require.EqualValues(t, 4, current.Generation)
	n, err = srv.Backend().AttachedClients(newID, current.SessionID)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	// A scheduled command superseded before SDK capture performs no RPC.
	var generation atomic.Uint64
	generation.Store(2)
	stale := ContextWithSessionSelection(ctx, newID, &generation, 1)
	require.ErrorContains(t, first.SetCurrentSession(stale, "stale-command"), "superseded")
	canceled, cancelSelection := context.WithCancel(ctx)
	cancelSelection()
	require.ErrorIs(t, first.SetCurrentSession(canceled, "canceled-command"), context.Canceled)
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
	for _, i := range []int{3, 4, 5} {
		require.Empty(t, selections[i].SessionID)
		require.EqualValues(t, 3, *selections[i].SelectionGeneration)
	}
}
