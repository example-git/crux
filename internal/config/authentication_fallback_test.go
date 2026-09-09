package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/shell"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func authenticationFallbackFixture(t *testing.T, template, key string, disabled bool) (*ConfigStore, string, env.Env) {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"),
		"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"),
		"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(ProviderProfileCoreOnly),
		"FALLBACK_KEY": key, "CRUX_DISABLE_AUTO_MEMORY": "true",
	}
	base := env.NewFromMap(values)
	manifest, err := os.ReadFile("../../plugins/provider-presets/deepseek.plugin/manifest.json")
	require.NoError(t, err)
	for field, value := range map[string]any{"id": "test.authentication.fallback", "preset.id": "authentication-fallback", "preset.api_key": template} {
		manifest, err = sjson.SetBytes(manifest, field, value)
		require.NoError(t, err)
	}
	bundle := filepath.Join(root, "fallback.plugin")
	require.NoError(t, os.MkdirAll(bundle, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), manifest, 0o600))
	installTrustedProviderBundle(t, values["CRUX_GLOBAL_DATA"], values["CRUX_CACHE_DIR"], bundle)
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	document := []byte(`{"providers":{"authentication-fallback":{"api_key":"synthetic-scoped-key","preset":{"id":"test.authentication.fallback"}}},"models":{"large":{"provider":"authentication-fallback","model":"deepseek-v4-pro"},"small":{"provider":"authentication-fallback","model":"deepseek-v4-flash"}}}`)
	document, err = sjson.SetBytes(document, "providers.authentication-fallback.disable", disabled)
	require.NoError(t, err)
	path := filepath.Join(workspace, "crux.json")
	require.NoError(t, os.WriteFile(path, document, 0o600))
	store, err := LoadIsolated(workspace, filepath.Join(root, "workspace-data"), false, base)
	require.NoError(t, err)
	return store, path, base
}

func authenticationFallbackEdit(t *testing.T, store *ConfigStore, path string, base env.Env) (AuthenticationCapture, authenticationCredentialEdit) {
	t.Helper()
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	layers, err := evaluateAuthenticationLayers(t.Context(), capture.inputs, base, nil, RuntimeOverrides{})
	require.NoError(t, err)
	edit, err := layers.stageCredentials(t.Context(), path, "authentication-fallback", nil)
	require.NoError(t, err)
	return capture, edit
}

func TestAuthenticationFallbackMatchesProductionReload(t *testing.T) {
	for _, test := range []struct{ name, template, key, want string }{
		{"captured environment", "$FALLBACK_KEY", "synthetic-captured-key", "synthetic-captured-key"},
		{"empty environment", "$FALLBACK_KEY", "", ""},
		{"empty catalog", "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, path, base := authenticationFallbackFixture(t, test.template, test.key, false)
			capture, edit := authenticationFallbackEdit(t, store, path, base)
			owner, ok := capture.runtime.ProviderOwner("authentication-fallback")
			require.True(t, ok)
			t.Setenv("FALLBACK_KEY", "synthetic-live-environment-must-not-be-used")
			capture.runtime.resolver = authenticationFallbackFailResolver{}
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			got, err := authenticationCredentialFallback(t.Context(), capture.runtime, edit.after, owner)
			require.NoError(t, err)
			require.Equal(t, test.want, got)
			err = validateAuthenticationLogoutFallback(t.Context(), capture.runtime, edit.after, owner)
			if test.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "would restore")
				require.NotContains(t, err.Error(), test.want)
			}
			unchanged, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, before, unchanged, "preflight must never write the scoped config")
			// This deliberate fixture-only write tests the actual loader's next
			// credential. It is not execution of the pending logout transaction.
			require.NoError(t, os.WriteFile(path, edit.data, 0o600))
			reloaded, err := LoadIsolated(store.workingDir, filepath.Join(filepath.Dir(store.workingDir), "workspace-data"), false, base)
			require.NoError(t, err)
			provider, configured := reloaded.Config().Providers.Get(owner.ProviderID)
			if test.want != "" {
				require.True(t, configured)
			}
			if configured {
				// The real loader retains the catalog template; provider
				// construction uses the accepted resolver to obtain its key.
				resolved, err := reloaded.RuntimeSnapshot().Resolve(provider.APIKey)
				require.NoError(t, err)
				require.Equal(t, test.want, resolved)
			} else {
				require.Empty(t, test.want)
			}
			require.Nil(t, provider.OAuthToken)
		})
	}
}

type authenticationFallbackFailResolver struct{}

func (authenticationFallbackFailResolver) ResolveValue(string) (string, error) {
	panic("logout fallback must not use the ordinary resolver")
}

func TestAuthenticationFallbackRejectsCommandsWithoutExecuting(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "command-must-not-run")
	command := "$(printf x >> '" + marker + "')"
	for _, template := range []string{command, "${FALLBACK_KEY:-" + command + "}", "${MISSING:+" + command + "}", "$"} {
		store, path, base := authenticationFallbackFixture(t, "$FALLBACK_KEY", "present", false)
		capture, edit := authenticationFallbackEdit(t, store, path, base)
		// Current preset schemas reject these templates at installation. This
		// private fixture exercises the fallback guard for any future catalog
		// source admitting commands; it is not a production discovery claim.
		capture.runtime.config = capture.runtime.config.cloneForWrite()
		scan := cloneProviderScan(*capture.runtime.config.providerScan)
		for i := range scan.Providers {
			if scan.Providers[i].ID == "authentication-fallback" {
				scan.Providers[i].APIKey = template
			}
		}
		capture.runtime.config.providerScan = &scan
		owner, ok := capture.runtime.ProviderOwner("authentication-fallback")
		require.True(t, ok)
		err := validateAuthenticationLogoutFallback(t.Context(), capture.runtime, edit.after, owner)
		require.ErrorContains(t, err, "without commands")
		require.NotContains(t, err.Error(), marker)
	}
	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestAuthenticationFallbackDisabledAndCapturedNounset(t *testing.T) {
	prior := shell.NoUnset.Load()
	shell.NoUnset.Store(false)
	t.Cleanup(func() { shell.NoUnset.Store(prior) })
	store, path, base := authenticationFallbackFixture(t, "$FALLBACK_KEY", "present", true)
	capture, edit := authenticationFallbackEdit(t, store, path, base)
	owner, ok := capture.runtime.ProviderOwner("authentication-fallback")
	require.True(t, ok)
	require.ErrorContains(t, validateAuthenticationLogoutFallback(t.Context(), capture.runtime, edit.after, owner), "would restore")
	provider, _ := store.Config().Providers.Get(owner.ProviderID)
	require.True(t, provider.Disable)

	store, path, base = authenticationFallbackFixture(t, "$ABSENT_FROM_CAPTURE", "", false)
	capture, edit = authenticationFallbackEdit(t, store, path, base)
	owner, ok = capture.runtime.ProviderOwner("authentication-fallback")
	require.True(t, ok)
	shell.NoUnset.Store(true)
	require.NoError(t, validateAuthenticationLogoutFallback(t.Context(), capture.runtime, edit.after, owner), "use the load's policy, not the current global toggle")
	shell.NoUnset.Store(false)
	changed := capture.runtime.config.cloneForWrite()
	changed.authenticationBasis = changed.authenticationBasis.clone()
	changed.authenticationBasis.noUnset = true
	capture.runtime.config = changed
	require.ErrorContains(t, validateAuthenticationLogoutFallback(t.Context(), capture.runtime, edit.after, owner), "without commands")
}

func TestAuthenticationFallbackRequiresExactCapturedAuthority(t *testing.T) {
	store, path, base := authenticationFallbackFixture(t, "$FALLBACK_KEY", "present", false)
	capture, edit := authenticationFallbackEdit(t, store, path, base)
	owner, ok := capture.runtime.ProviderOwner("authentication-fallback")
	require.True(t, ok)
	wrong := owner
	wrong.PresetVersion = "replaced"
	require.ErrorContains(t, validateAuthenticationLogoutFallback(t.Context(), capture.runtime, edit.after, wrong), "owner changed")
	for _, change := range []struct {
		name  string
		apply func(*Config)
		want  string
	}{
		{"no catalog", func(c *Config) { c.providerScan = nil }, "owner changed"},
		{"no basis", func(c *Config) { c.authenticationBasis = nil }, "no accepted input basis"},
		{"ambiguous catalog", func(c *Config) {
			c.providerScan.Providers = append(c.providerScan.Providers, c.providerScan.Providers[len(c.providerScan.Providers)-1])
		}, "catalog is ambiguous"},
	} {
		t.Run(change.name, func(t *testing.T) {
			copy := capture.runtime
			copy.config = copy.config.cloneForWrite()
			scan := cloneProviderScan(*copy.config.providerScan)
			copy.config.providerScan = &scan
			change.apply(copy.config)
			require.ErrorContains(t, validateAuthenticationLogoutFallback(t.Context(), copy, edit.after, owner), change.want)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, validateAuthenticationLogoutFallback(ctx, capture.runtime, edit.after, owner), context.Canceled)
	// Explicitly disabled default providers use the real loader's custom-only
	// branch and do not consult even a retained catalog credential.
	copy := capture.runtime
	copy.config = copy.config.cloneForWrite()
	copy.config.Options.DisableDefaultProviders = true
	require.NoError(t, validateAuthenticationLogoutFallback(t.Context(), copy, edit.after, owner))
	copy = capture.runtime
	copy.environment = nil
	require.True(t, errors.Is(validateAuthenticationLogoutFallback(t.Context(), copy, edit.after, owner), errAuthenticationBasisUnavailable))
	data, err := json.Marshal(store.Config())
	require.NoError(t, err)
	require.NotContains(t, string(data), "authenticationBasis")
}
