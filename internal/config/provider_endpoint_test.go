package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/stretchr/testify/require"
)

func TestCheckedEndpointLifetimeAndEarlyValidation(t *testing.T) {
	host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	f := newCheckedAPIKeyFixture(t, host.URL, ScopeGlobal)
	preparation, err := f.store.PrepareCheckedAPIKey(t.Context(), f.capture(t), f.owner, "provider.api_key", "synthetic-new")
	require.NoError(t, err)
	result, err := f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, preparation)
	require.NoError(t, err)
	snapshot := result.After.runtime
	original, _ := snapshot.Config().Providers.Get("checked")
	require.NoError(t, f.store.SetProviderAPIKey(ScopeGlobal, "checked", ProviderAPIKeyCredential{Owner: f.owner, APIKey: "synthetic-replaced"}))
	current := f.store.RuntimeSnapshot()
	provider, _ := current.Config().Providers.Get("checked")
	require.Nil(t, provider.resolvedAPIKey)
	require.NotNil(t, provider.resolvedEndpoint)
	endpoint, err := current.ResolveProviderEndpoint(provider)
	require.NoError(t, err)
	require.Equal(t, host.URL, endpoint)
	require.NoError(t, f.store.SetCompactMode(ScopeGlobal, true))

	provider, _ = f.store.Config().Providers.Get("checked")
	require.NotNil(t, provider.resolvedEndpoint)
	require.NoError(t, f.store.ReloadFromDisk(t.Context()))
	reloaded := f.store.RuntimeSnapshot()
	provider, _ = reloaded.Config().Providers.Get("checked")
	require.Nil(t, provider.resolvedEndpoint)
	_, err = reloaded.ResolveProviderEndpoint(original)
	require.ErrorIs(t, err, errResolvedProviderEndpointStale)
	endpoint, err = snapshot.ResolveProviderEndpoint(original)
	require.NoError(t, err)
	require.Equal(t, host.URL, endpoint)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d"} {
		require.NotContains(t, fmt.Sprintf(verb, *original.resolvedEndpoint), host.URL)
	}
	_, err = json.Marshal(original.resolvedEndpoint)
	require.Error(t, err)
	redacted, _ := snapshot.Config().RedactedForTransport().Providers.Get("checked")
	require.Nil(t, redacted.resolvedEndpoint)
	// A stale endpoint, even with no credential marker, fails before headers run.
	changed := snapshot.Config().cloneForWrite()
	provider = cloneProviderConfig(original)
	provider.resolvedAPIKey = nil
	provider.BaseURL = "https://changed.invalid"
	marker := filepath.Join(t.TempDir(), "header")
	provider.ExtraHeaders = map[string]string{"X-Run": fmt.Sprintf("$(printf x > '%s')", marker)}
	changed.Providers.Set("checked", provider)
	err = changed.configureProvidersWithMigration(context.Background(), f.store, env.New(), NewShellVariableResolver(env.New()), nil, nil)
	require.ErrorIs(t, err, errResolvedProviderEndpointStale)
	_, err = os.Stat(marker)
	require.True(t, os.IsNotExist(err))
}
