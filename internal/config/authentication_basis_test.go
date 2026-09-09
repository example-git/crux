package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/shell"
	"github.com/stretchr/testify/require"
)

func authenticationBasisStore(t *testing.T, prepare func(string, map[string]string)) (*ConfigStore, string, env.Env) {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"),
		"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"),
		"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(ProviderProfileCoreOnly),
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), []byte(`{
"options":{"disable_default_providers":true},
"providers":{"fixture":{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"synthetic-secret","models":[{"id":"main","default_max_tokens":100},{"id":"small","default_max_tokens":50}]}},
"models":{"large":{"provider":"fixture","model":"main"},"small":{"provider":"fixture","model":"small"}}}`), 0o600))
	if prepare != nil {
		prepare(root, values)
	}
	base := env.NewFromMap(values)
	store, err := LoadIsolated(root, filepath.Join(root, "workspace"), false, base)
	require.NoError(t, err)
	return store, root, base
}

func authenticationBasisCapture(t *testing.T, store *ConfigStore, base env.Env) (AuthenticationCapture, authenticationLayers) {
	t.Helper()
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	layers, err := evaluateAuthenticationLayers(t.Context(), capture.inputs, base, store.ephemeralProviderSnapshot(), store.Overrides())
	require.NoError(t, err)
	return capture, layers
}

func authenticationBasisWriteField(t *testing.T, path string, keys []string, value string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data, err = runtimeControlChangeField(data, keys, json.RawMessage(value), false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func TestAuthenticationBasisLoadedPrivateStableAndMigration(t *testing.T) {
	store, root, base := authenticationBasisStore(t, func(root string, _ map[string]string) {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "config"), 0o700))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "config", "crux.json"), []byte(`{"options":{"disable_notifications":true}}`), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(root, "data", "crux.json"), []byte(`{}`), 0o600))
		// Startup corrects this selected model and adds exact owner markers.
		authenticationBasisWriteField(t, filepath.Join(root, "crux.json"), []string{"models", "large", "model"}, `"missing"`)
	})
	basis := store.Config().authenticationBasis
	require.NotNil(t, basis)
	require.True(t, basis.valid)
	require.Contains(t, string(basis.configured), "synthetic-secret")
	for _, value := range []any{basis, *basis, basis.sources[filepath.Join(root, "crux.json")]} {
		_, err := json.Marshal(value)
		require.Error(t, err)
		for _, format := range []string{"%v", "%+v", "%#v"} {
			text := fmt.Sprintf(format, value)
			require.NotContains(t, text, "synthetic-secret")
			require.NotContains(t, text, root)
		}
	}
	encoded, err := json.Marshal(store.Config())
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "authenticationBasis")
	var decoded Config
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Nil(t, decoded.authenticationBasis)
	capture, layers := authenticationBasisCapture(t, store, base)
	require.NoError(t, capture.validateConfigBasis(layers, ""), "an immediate mutation must accept actual startup correction receipts")
	global := basis.sources[store.globalDataPath]
	require.Contains(t, string(global.raw), `"notifications":"disabled"`)
	require.Contains(t, string(global.raw), `"owner"`)
	require.Contains(t, string(global.raw), `"model"`)
	require.Equal(t, basis, store.Config().cloneForWrite().authenticationBasis)
	missing := basis.sources[store.workspacePath]
	require.False(t, missing.exists)
	require.Empty(t, missing.raw)
	before := store.RuntimeSnapshot()
	for range 2 {
		current, currentLayers := authenticationBasisCapture(t, store, base)
		require.NoError(t, current.validateConfigBasis(currentLayers, ""))
		require.True(t, capture.SameObservation(current))
	}
	require.True(t, before.SamePublication(store.RuntimeSnapshot()))
}

func TestAuthenticationBasisFreshCaptureDoesNotAcceptPriorDiskEdit(t *testing.T) {
	store, root, base := authenticationBasisStore(t, nil)
	old := store.Config()
	path := filepath.Join(root, "crux.json")
	authenticationBasisWriteField(t, path, []string{"providers", "fixture", "extra_headers"}, `{"X-New":"unaccepted"}`)
	first, layers := authenticationBasisCapture(t, store, base)
	require.ErrorIs(t, first.validateConfigBasis(layers, "fixture"), errAuthenticationBasisChanged)
	second, nextLayers := authenticationBasisCapture(t, store, base)
	require.True(t, first.SameObservation(second), "even a stable fresh public observation is not acceptance")
	require.ErrorIs(t, second.validateConfigBasis(nextLayers, "fixture"), errAuthenticationBasisChanged)
	require.Same(t, old, store.Config())
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	after, afterLayers := authenticationBasisCapture(t, store, base)
	require.NoError(t, after.validateConfigBasis(afterLayers, "fixture"))
	provider, _ := store.Config().Providers.Get("fixture")
	require.Equal(t, "unaccepted", provider.ExtraHeaders["X-New"])
}

func TestAuthenticationBasisTypedWriteDoesNotBlessPeerEdits(t *testing.T) {
	store, _, base := authenticationBasisStore(t, nil)
	before := store.Config()
	baseline := bytes.Clone(before.authenticationBasis.sources[store.globalDataPath].raw)
	authenticationBasisWriteField(t, store.globalDataPath, []string{"foreign", "precise"}, `9007199254740993`)
	require.ErrorContains(t, store.SetConfigField(ScopeGlobal, "options.tui.delivery_mode", "steer"), "config file updated but failed to publish")
	require.Same(t, before, store.Config(), "unverified postimages cannot publish a replacement config")
	require.Equal(t, baseline, before.authenticationBasis.sources[store.globalDataPath].raw, "retained bases are immutable")
	require.NotContains(t, string(store.Config().authenticationBasis.sources[store.globalDataPath].raw), "foreign")
	disk, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.Contains(t, string(disk), "9007199254740993", "the peer edit remains on disk")
	var compact bytes.Buffer
	require.NoError(t, json.Compact(&compact, disk))
	require.Contains(t, compact.String(), `"delivery_mode":"steer"`, "the error reports the durable typed write")
	capture, layers := authenticationBasisCapture(t, store, base)
	require.ErrorIs(t, capture.validateConfigBasis(layers, ""), errAuthenticationBasisChanged)
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	capture, layers = authenticationBasisCapture(t, store, base)
	require.NoError(t, capture.validateConfigBasis(layers, ""))
	// The normal authored typed write is fully maintained after acceptance.
	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.tui.delivery_mode", "queue"))
	capture, layers = authenticationBasisCapture(t, store, base)
	require.NoError(t, capture.validateConfigBasis(layers, ""))
}

func TestAuthenticationBasisHeaderAndShellProvenanceNoExtraExecution(t *testing.T) {
	store, root, base := authenticationBasisStore(t, func(root string, values map[string]string) {
		values["HEADER_MARKER"] = filepath.Join(root, "header-marker")
		values["SHELL_MARKER"] = filepath.Join(root, "shell-marker")
		authenticationBasisWriteField(t, filepath.Join(root, "crux.json"), []string{"providers", "fixture", "extra_headers"}, `{"X-Source":"$(printf x >> \"$HEADER_MARKER\"; printf admitted)"}`)
		require.NoError(t, os.WriteFile(filepath.Join(root, "cruxrc"), []byte("printf x >> \"$SHELL_MARKER\"\nprovider add fixture --api-key synthetic-secret --base-url https://example.invalid/v1\n"), 0o600))
	})
	readMarker := func(name string) string {
		data, err := os.ReadFile(filepath.Join(root, name))
		require.NoError(t, err)
		return string(data)
	}
	require.Equal(t, "x", readMarker("shell-marker"), "load basis must reuse the original shell evaluation")
	require.Equal(t, "x", readMarker("header-marker"))
	provider, _ := store.Config().Providers.Get("fixture")
	require.Equal(t, "admitted", provider.ExtraHeaders["X-Source"])
	require.Contains(t, string(store.Config().authenticationBasis.sources[filepath.Join(root, "crux.json")].raw), "HEADER_MARKER")
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.Equal(t, "x", readMarker("shell-marker"), "status does not evaluate shell sources")
	layers, err := evaluateAuthenticationLayers(t.Context(), capture.inputs, base, nil, RuntimeOverrides{})
	require.NoError(t, err)
	require.Equal(t, "xx", readMarker("shell-marker"), "mutation preparation evaluates each shell source once")
	for range 2 {
		require.NoError(t, capture.validateConfigBasis(layers, "fixture"))
	}
	require.Equal(t, "xx", readMarker("shell-marker"))
	require.Equal(t, "x", readMarker("header-marker"), "pure admission must not rerun resolved header expressions")
	authenticationBasisWriteField(t, filepath.Join(root, "crux.json"), []string{"providers", "fixture", "extra_headers", "X-Source"}, `"$(printf should-not-run >> \"$HEADER_MARKER\"; printf admitted)"`)
	capture, layers = authenticationBasisCapture(t, store, base)
	require.ErrorIs(t, capture.validateConfigBasis(layers, "fixture"), errAuthenticationBasisChanged)
	require.Equal(t, "x", readMarker("header-marker"))
}

func TestAuthenticationBasisPreservesDiscoveryInputWithoutRediscovery(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"main","object":"model"},{"id":"small","object":"model"},{"id":"discovered","object":"model"}]}`))
	}))
	defer server.Close()
	store, root, base := authenticationBasisStore(t, func(root string, _ map[string]string) {
		data, err := json.Marshal(server.URL + "/v1")
		require.NoError(t, err)
		path := filepath.Join(root, "crux.json")
		authenticationBasisWriteField(t, path, []string{"providers", "fixture", "base_url"}, string(data))
		authenticationBasisWriteField(t, path, []string{"providers", "fixture", "discover_models"}, `true`)
	})
	require.Positive(t, calls.Load())
	loadedCalls := calls.Load()
	provider, _ := store.Config().Providers.Get("fixture")
	require.NotNil(t, store.Config().GetModel("fixture", "discovered"))
	require.NotContains(t, string(store.Config().authenticationBasis.sources[filepath.Join(root, "crux.json")].raw), "discovered")
	require.Greater(t, len(provider.Models), 2)
	capture, layers := authenticationBasisCapture(t, store, base)
	require.NoError(t, capture.validateConfigBasis(layers, ""))
	require.Equal(t, loadedCalls, calls.Load())
}

func TestAuthenticationBasisConfiguredSnapshotUsesActualDefaultOrder(t *testing.T) {
	store, _, base := authenticationBasisStore(t, func(root string, _ map[string]string) {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
		require.NoError(t, os.MkdirAll(filepath.Join(root, "workspace"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(root, "data", "crux.json"), []byte(`{"options":{"tui":{"delivery_mode":"steer"}}}`), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(root, "workspace", "crux.json"), []byte(`{"options":{"tui":{"delivery_mode":"queue"},"context_paths":["workspace.md"]}}`), 0o600))
	})
	var configured Config
	require.NoError(t, json.Unmarshal(store.Config().authenticationBasis.configured, &configured))
	require.Equal(t, "steer", configured.Options.TUI.DeliveryMode, "global delivery preference is consumed after workspace merging")
	require.Contains(t, configured.Options.ContextPaths, "workspace.md")
	require.Contains(t, configured.Options.ContextPaths, "AGENTS.md")
	require.Equal(t, store.Config().Options.ContextPaths, configured.Options.ContextPaths)
	require.Equal(t, store.Config().Options.SkillsPaths, configured.Options.SkillsPaths)
	capture, layers := authenticationBasisCapture(t, store, base)
	require.NoError(t, capture.validateConfigBasis(layers, ""))
}

func TestAuthenticationBasisCredentialExemptionAndLiteralReceipt(t *testing.T) {
	store, root, base := authenticationBasisStore(t, nil)
	path := filepath.Join(root, "crux.json")
	authenticationBasisWriteField(t, path, []string{"providers", "fixture", "api_key"}, `"peer-target-key"`)
	capture, layers := authenticationBasisCapture(t, store, base)
	require.ErrorIs(t, capture.validateConfigBasis(layers, ""), errAuthenticationBasisChanged)
	require.NoError(t, capture.validateConfigBasis(layers, "fixture"), "a targeted credential repair is allowed")
	authenticationBasisWriteField(t, path, []string{"providers", "fixture", "disable"}, `true`)
	capture, layers = authenticationBasisCapture(t, store, base)
	require.ErrorIs(t, capture.validateConfigBasis(layers, "fixture"), errAuthenticationBasisChanged)
	// Exercise dotted IDs directly through the same literal receipt used by
	// stageCredentials, preserving a neighboring nested key and foreign data.
	basis := newAuthenticationLoadBasis()
	basis.source(path, []byte(`{"providers":{"exact.id":{"api_key":"old"},"exact":{"id":{"api_key":"neighbor"}}},"foreign":9007199254740993}`), nil, true)
	cfg := &Config{authenticationBasis: basis}
	desired := authenticationLayerDesired("exact.id", "new")
	cfg.advanceAuthenticationBasisCredentials(path, "exact.id", &desired)
	value, err := runtimeControlReadField(cfg.authenticationBasis.sources[path].raw, []string{"providers", "exact.id", "api_key"})
	require.NoError(t, err)
	require.JSONEq(t, `"new"`, string(value.Value))
	require.Contains(t, string(cfg.authenticationBasis.sources[path].raw), "neighbor")
	require.Contains(t, string(cfg.authenticationBasis.sources[path].raw), "9007199254740993")
	require.Contains(t, string(basis.sources[path].raw), `"old"`)
	cfg.advanceAuthenticationBasisCredentials(path, "exact.id", nil)
	value, err = runtimeControlReadField(cfg.authenticationBasis.sources[path].raw, []string{"providers", "exact.id", "api_key"})
	require.NoError(t, err)
	require.False(t, value.Present)
}

func TestAuthenticationBasisPolicyAndUnavailableInputs(t *testing.T) {
	previous := shell.NoUnset.Load()
	t.Cleanup(func() { shell.NoUnset.Store(previous) })
	shell.NoUnset.Store(true)
	basis := newAuthenticationLoadBasis()
	shell.NoUnset.Store(false)
	require.True(t, basis.noUnset)
	manual := AuthenticationCapture{runtime: NewTestStore(&Config{}).RuntimeSnapshot()}
	require.ErrorIs(t, manual.validateConfigBasis(authenticationLayers{}, ""), errAuthenticationBasisUnavailable)
	detached := AuthenticationCapture{runtime: RuntimeSnapshot{clientRuntime: &clientRuntimeState{}}}
	require.ErrorIs(t, detached.validateConfigBasis(authenticationLayers{}, ""), ErrClientRuntimeManaged)
	store, root, base := authenticationBasisStore(t, nil)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".crux.json"), []byte(`{"foreign":true}`), 0o600))
	capture, layers := authenticationBasisCapture(t, store, base)
	require.ErrorIs(t, capture.validateConfigBasis(layers, ""), errAuthenticationBasisChanged)
	_, err := evaluateAuthenticationLayers(context.Background(), authenticationConfigInputs{}, base, nil, RuntimeOverrides{})
	require.Error(t, err)
}

func TestAuthenticationBasisCredentialReceiptMatchesScopedOAuthClient(t *testing.T) {
	inputs, paths := authenticationLayerFixture(t, []string{"scope.json"}, []string{`{"providers":{"exact.id":{"api_key":"old","oauth":{"access_token":"old","client":{"client_id":"old-id","client_secret":"old-secret","auth_style":2}}}}}`})
	layers, err := evaluateAuthenticationLayers(t.Context(), inputs, env.NewFromMap(nil), nil, RuntimeOverrides{})
	require.NoError(t, err)
	desired := authenticationLayerDesired("exact.id", "new")
	desired.OAuthToken.Client = &oauth.OAuthClient{ClientID: "new-id"}
	edit, err := layers.stageCredentials(t.Context(), paths[0], "exact.id", &desired)
	require.NoError(t, err)
	basis := newAuthenticationLoadBasis()
	basis.source(paths[0], inputs.files[0].data, inputs.files[0].data, true)
	cfg := &Config{authenticationBasis: basis}
	cfg.advanceAuthenticationBasisCredentials(paths[0], "exact.id", &desired)
	require.True(t, RuntimeControlJSONEqual(edit.data, cfg.authenticationBasis.sources[paths[0]].raw))
	for _, field := range []string{"client_secret", "auth_url", "token_url", "auth_style"} {
		value, err := runtimeControlReadField(cfg.authenticationBasis.sources[paths[0]].raw, []string{"providers", "exact.id", "oauth", "client", field})
		require.NoError(t, err)
		require.True(t, value.Present, "empty OAuth client fields must be explicit in both write and receipt")
	}
}
