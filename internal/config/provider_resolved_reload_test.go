package config

import (
	"context"
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
	require.NoError(t, os.WriteFile(f.path, data, 0o600))
	err = f.store.SetConfigField(ScopeGlobal, "options.disable_auto_summarize", true)
	require.ErrorIs(t, err, errResolvedProviderAPIKeyStale)
	require.NoFileExists(t, marker)
	// An explicit reload is the user's separate source-resolution action.
	require.NoError(t, f.store.ReloadFromDisk(t.Context()))
	require.FileExists(t, marker)
}

func TestCheckedAPIKeyGenericSettingRejectsPreparerSourceChanges(t *testing.T) {
	for _, change := range []string{"key", "endpoint", "key-removal", "provider-removal", "unchanged"} {
		t.Run(change, func(t *testing.T) {
			host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
			f := newCheckedAPIKeyFixture(t, host.URL, ScopeGlobal)
			preparation, err := f.store.PrepareCheckedAPIKey(t.Context(), f.capture(t), f.owner, "provider.api_key", "synthetic-checked")
			require.NoError(t, err)
			_, err = f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, preparation)
			require.NoError(t, err)
			original := f.store.Config()
			var commits, aborts int
			var peer []byte
			path := f.path
			if change == "endpoint" {
				path = filepath.Join(f.root, "config", "crux.json")
			}
			marker := filepath.Join(f.root, "peer-source-must-not-execute")
			f.store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				switch change {
				case "key":
					data, err = sjson.SetBytes(data, "providers.checked.api_key", fmt.Sprintf("$(printf x > '%s'; printf peer-key)", marker))
				case "endpoint":
					data, err = sjson.SetBytes(data, "providers.checked.base_url", fmt.Sprintf("$(printf x > '%s'; printf https://peer.invalid)", marker))
				case "key-removal":
					data, err = sjson.DeleteBytes(data, "providers.checked.api_key")
				case "provider-removal":
					data, err = sjson.DeleteBytes(data, "providers.checked")
				}
				require.NoError(t, err)
				if change != "unchanged" {
					require.NoError(t, os.WriteFile(path, data, 0o600))
					peer = data
				}
				return RuntimeGenerationCandidate{Commit: func() { commits++ }, Abort: func() {
					if commits == 0 {
						aborts++
					}
				}}, nil
			})
			err = f.store.SetConfigField(ScopeGlobal, "options.disable_auto_summarize", true)
			if change == "unchanged" {
				require.NoError(t, err)
				require.Equal(t, 1, commits)
				require.Zero(t, aborts)
			} else {
				require.ErrorIs(t, err, errAuthenticationInputsChanged)
				require.Zero(t, commits)
				require.Equal(t, 1, aborts)
				require.Same(t, original, f.store.Config())
				current, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				require.Equal(t, peer, current)
			}
			require.NoFileExists(t, marker)
		})
	}
}

func TestCheckedAPIKeyGenericSettingAllowsOwnStartupReceipts(t *testing.T) {
	for _, change := range []string{"notifications", "new-owner"} {
		t.Run(change, func(t *testing.T) {
			host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
			f := newCheckedAPIKeyFixture(t, host.URL, ScopeGlobal)
			preparation, err := f.store.PrepareCheckedAPIKey(t.Context(), f.capture(t), f.owner, "provider.api_key", "synthetic-checked")
			require.NoError(t, err)
			_, err = f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, preparation)
			require.NoError(t, err)
			fields := map[string]any{"options.disable_notifications": true}
			if change == "new-owner" {
				fields = map[string]any{"providers.added": map[string]any{"type": "openai-compat", "base_url": "https://example.invalid", "api_key": "synthetic-added", "models": []map[string]string{{"id": "added"}}}}
			}
			require.NoError(t, f.store.SetConfigFields(ScopeGlobal, fields))
			provider, _ := f.store.Config().Providers.Get("checked")
			require.NotNil(t, provider.resolvedAPIKey)
			require.NotNil(t, provider.resolvedEndpoint)
			if change == "notifications" {
				require.Equal(t, "disabled", f.store.Config().Options.Notifications)
			} else {
				added, ok := f.store.Config().Providers.Get("added")
				require.True(t, ok)
				require.NotNil(t, added.Owner)
			}
		})
	}
}
