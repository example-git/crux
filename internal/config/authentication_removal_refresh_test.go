package config

import (
	"context"
	"fmt"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthenticationRemoveRefreshesExactSuccessorOverHTTPS(t *testing.T) {
	var exchanges atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "captured-client", r.Form.Get("client_id"))
		require.Equal(t, "synthetic-inactive-refresh", r.Form.Get("refresh_token"))
		_, _ = fmt.Fprint(w, `{"access_token":"synthetic-refreshed-access","refresh_token":"synthetic-refreshed-refresh","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)
	previous := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	f := newAuthenticationCandidateFixture(t, "example-responses", false, false, server.URL+"/token")
	t.Setenv("AI_CLI_DIR", filepath.Join(f.root, "accounts"))
	first := accounts.Entry{ID: "active", AccessToken: "synthetic-current", RefreshToken: "synthetic-current-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	second := accounts.Entry{ID: "inactive", AccessToken: "synthetic-expired", RefreshToken: "synthetic-inactive-refresh", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
	require.NoError(t, accounts.Save(t.Context(), f.owner.AccountNamespace, first))
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, second))
	before, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	old := f.store.Config()
	f.store.SetRuntimeGenerationPreparer(func(ctx context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		active, err := accounts.Active(ctx, f.owner.AccountNamespace)
		require.NoError(t, err)
		require.Equal(t, "active", active.ID)
		prospective, captured, err := snapshot.CapturedConstructionAccount(f.owner)
		require.NoError(t, err)
		require.True(t, captured)
		require.Equal(t, "synthetic-refreshed-access", prospective.AccessToken)
		return RuntimeGenerationCandidate{Commit: func() {}, Abort: func() {}}, nil
	})
	result, err := f.store.RemoveAuthenticationAccount(t.Context(), ScopeGlobal, before, f.owner, "active")
	require.NoError(t, err)
	require.True(t, result.AccountRefreshed && result.AccountsSaved && result.ConfigSaved && result.RuntimePublished)
	require.Equal(t, int32(1), exchanges.Load())
	remaining, e := accounts.List(t.Context(), f.owner.AccountNamespace)
	require.NoError(t, e)
	require.Len(t, remaining, 1)
	require.Equal(t, "inactive", remaining[0].ID)
	current, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.True(t, result.After.SameObservation(current))
	require.Equal(t, old.Models, f.store.Config().Models)
	require.Equal(t, old.Agents, f.store.Config().Agents)
	provider, ok := f.store.Config().Providers.Get(f.owner.ProviderID)
	require.True(t, ok)
	require.Equal(t, "synthetic-refreshed-access", provider.APIKey)
	count, err := os.ReadFile(f.marker)
	require.NoError(t, err)
	require.Equal(t, "x", string(count), "unconfigured candidate headers evaluate once")
}
