package config

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestCheckedAPIKeyGenericSettingsRetainResolvedInputs(t *testing.T) {
	host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	root := t.TempDir()
	keyCounter, endpointCounter := filepath.Join(root, "key"), filepath.Join(root, "endpoint")
	endpoint := fmt.Sprintf("$(printf x >> '%s'; printf '%%s' '%s')", endpointCounter, host.URL)
	f := newCheckedAPIKeyFixture(t, endpoint, ScopeGlobal)
	require.NoError(t, os.Remove(endpointCounter))
	source := fmt.Sprintf("$(printf x >> '%s'; printf 'synthetic-$LITERAL')", keyCounter)
	preparation, err := f.store.PrepareCheckedAPIKey(t.Context(), f.capture(t), f.owner, "provider.api_key", source)
	require.NoError(t, err)
	result, err := f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, preparation)
	require.NoError(t, err)
	require.NoError(t, f.store.SetConfigField(ScopeGlobal, "options.disable_auto_summarize", true))
	provider, _ := f.store.Config().Providers.Get("checked")
	require.NotNil(t, provider.resolvedAPIKey)
	require.NotNil(t, provider.resolvedEndpoint)
	key, err := f.store.RuntimeSnapshot().ResolveProviderAPIKey(provider)
	require.NoError(t, err)
	require.Equal(t, "synthetic-$LITERAL", key)
	url, err := f.store.RuntimeSnapshot().ResolveProviderEndpoint(provider)
	require.NoError(t, err)
	require.Equal(t, host.URL, url)
	_, err = f.store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	for _, path := range []string{keyCounter, endpointCounter} {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "x", string(data))
	}
	require.NoError(t, f.store.SetConfigField(ScopeGlobal, "providers.checked.api_key", source))
	provider, _ = f.store.Config().Providers.Get("checked")
	require.Nil(t, provider.resolvedAPIKey)
	require.NotNil(t, provider.resolvedEndpoint)
	data, err := os.ReadFile(keyCounter)
	require.NoError(t, err)
	require.Equal(t, "xx", string(data))
	data, err = os.ReadFile(endpointCounter)
	require.NoError(t, err)
	require.Equal(t, "x", string(data))
	old, _ := result.After.runtime.config.Providers.Get("checked")
	_, err = result.After.runtime.ResolveProviderAPIKey(old)
	require.NoError(t, err)
}
func TestResolvedInputsRecognizeExactGenericFieldWrites(t *testing.T) {
	for _, test := range []struct {
		key       string
		value     any
		unchanged bool
	}{
		{"options.foo", true, true},
		{`providers.owner\.with\.dots.extra_headers.X`, "changed", true},
		{`providers.owner\.with\.dots.api_key`, "private-resolved-input-one", false},
		{`providers.owner\.with\.dots`, map[string]any{"api_key": "same"}, false},
		{"providers", map[string]any{}, false},
	} {
		t.Run(test.key, func(t *testing.T) {
			result, err := resolvedInputFieldsUntouched("owner.with.dots", map[string]any{test.key: test.value})
			require.NoError(t, err)
			require.Equal(t, test.unchanged, result["api_key"])
		})
	}
}

func TestCheckedAPIKeyGenericSettingRejectsPeerCredentialBeforeExecution(t *testing.T) {
	host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	f := newCheckedAPIKeyFixture(t, host.URL, ScopeGlobal)
	preparation, err := f.store.PrepareCheckedAPIKey(t.Context(), f.capture(t), f.owner, "provider.api_key", "synthetic-checked")
	require.NoError(t, err)
	_, err = f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, preparation)
	require.NoError(t, err)
	marker := filepath.Join(f.root, "peer-source-must-not-execute")
	data, err := os.ReadFile(f.path)
	require.NoError(t, err)
	data, err = sjson.SetBytes(data, "providers.checked.api_key", fmt.Sprintf("$(printf x > '%s'; printf synthetic-peer)", marker))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.path, data, 0600))
	err = f.store.SetConfigField(ScopeGlobal, "options.disable_auto_summarize", true)
	require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
	require.NoFileExists(t, marker)
	// An explicit reload is the user's separate source-resolution action.
	require.NoError(t, f.store.ReloadFromDisk(t.Context()))
	require.FileExists(t, marker)
}
