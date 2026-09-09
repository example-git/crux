package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func authenticationRevocationConfig() *Config {
	return &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"copilot": {ID: "copilot", Type: catalog.TypeOpenAICompat,
			Owner: &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: providerregistry.ConstructionCopilot}},
	})}
}

func TestAuthenticationRevocationIsPrivateExactAndCopyOnWrite(t *testing.T) {
	store := NewTestStore(authenticationRevocationConfig())
	owner, ok := store.RuntimeSnapshot().ProviderOwner("copilot")
	require.True(t, ok)
	marked, err := store.Config().WithAuthenticationRevocation(owner)
	require.NoError(t, err)
	require.NoError(t, store.RuntimeSnapshot().AuthenticationRevocation(owner.ProviderID))
	require.ErrorIs(t, NewTestStore(marked).RuntimeSnapshot().AuthenticationRevocation(owner.ProviderID), ErrAuthenticationRevoked)
	clone := marked.cloneForWrite()
	delete(clone.authenticationRevocations, owner.ProviderID)
	require.ErrorIs(t, NewTestStore(marked).RuntimeSnapshot().AuthenticationRevocation(owner.ProviderID), ErrAuthenticationRevoked)
	require.NoError(t, NewTestStore(clone).RuntimeSnapshot().AuthenticationRevocation(owner.ProviderID))

	before, err := json.Marshal(store.Config())
	require.NoError(t, err)
	after, err := json.Marshal(marked)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after), "logout provenance must not be accepted from or emitted onto JSON")
	var wire Config
	require.NoError(t, json.Unmarshal(after, &wire))
	require.NoError(t, NewTestStore(&wire).RuntimeSnapshot().AuthenticationRevocation(owner.ProviderID))

	replaced := marked.cloneForWrite()
	provider, _ := replaced.Providers.Get(owner.ProviderID)
	provider.Owner = &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}
	replaced.Providers.Set(owner.ProviderID, provider)
	require.NoError(t, NewTestStore(replaced).RuntimeSnapshot().AuthenticationRevocation(owner.ProviderID))
	_, err = replaced.WithAuthenticationRevocation(owner)
	require.ErrorContains(t, err, "exact owner")
}

func TestAuthenticationRevocationOnlyAppliesWithoutCredentials(t *testing.T) {
	store := NewTestStore(authenticationRevocationConfig())
	owner, ok := store.RuntimeSnapshot().ProviderOwner("copilot")
	require.True(t, ok)
	marked, err := store.Config().WithAuthenticationRevocation(owner)
	require.NoError(t, err)
	for _, test := range []struct {
		name   string
		mutate func(*ProviderConfig)
	}{
		{"api key", func(p *ProviderConfig) { p.APIKey = "synthetic-new-key" }},
		{"api key template", func(p *ProviderConfig) { p.APIKeyTemplate = "$SYNTHETIC_KEY" }},
		{"oauth", func(p *ProviderConfig) { p.OAuthToken = &oauth.Token{AccessToken: "synthetic-new-oauth"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			updated := marked.cloneForWrite()
			provider, _ := updated.Providers.Get(owner.ProviderID)
			test.mutate(&provider)
			updated.Providers.Set(owner.ProviderID, provider)
			require.NoError(t, NewTestStore(updated).RuntimeSnapshot().AuthenticationRevocation(owner.ProviderID))
			_, err := updated.WithAuthenticationRevocation(owner)
			require.ErrorContains(t, err, "still has effective credentials")
			cleared := updated.cloneForWrite()
			provider, _ = cleared.Providers.Get(owner.ProviderID)
			provider.APIKey, provider.APIKeyTemplate, provider.OAuthToken = "", "", nil
			cleared.Providers.Set(owner.ProviderID, provider)
			require.Empty(t, cleared.authenticationRevocations)
			require.NoError(t, NewTestStore(cleared).RuntimeSnapshot().AuthenticationRevocation(owner.ProviderID), "a later credential removal cannot revive an old logout")
		})
	}
	disabled := marked.cloneForWrite()
	provider, _ := disabled.Providers.Get(owner.ProviderID)
	provider.Disable = true
	disabled.Providers.Set(owner.ProviderID, provider)
	require.ErrorIs(t, NewTestStore(disabled).RuntimeSnapshot().AuthenticationRevocation(owner.ProviderID), ErrAuthenticationRevoked)
	_, err = disabled.WithAuthenticationRevocation(owner)
	require.NoError(t, err, "explicit logout cleanup must not require an enabled provider")
}

func TestAuthenticationRevocationSurvivesUnrelatedReloadAndRetiresOnCredential(t *testing.T) {
	root := t.TempDir()
	base := env.NewFromMap(map[string]string{
		"HOME": root, "AI_CLI_DIR": filepath.Join(root, "accounts"),
		"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"),
		"CRUX_GLOBAL_DATA":   filepath.Join(root, "data"),
		"CRUX_CACHE_DIR":     filepath.Join(root, "cache"),
	})
	path := filepath.Join(root, "crux.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"options":{"disable_default_providers":true},"providers":{"fixture":{"type":"openai-compat","base_url":"https://example.invalid/v1","models":[{"id":"main"},{"id":"small"}]}},"models":{"large":{"provider":"fixture","model":"main"},"small":{"provider":"fixture","model":"small"}}}`), 0o600))
	store, err := LoadIsolated(root, filepath.Join(root, "workspace-data"), false, base)
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("fixture")
	require.True(t, ok)
	marked, err := store.Config().WithAuthenticationRevocation(owner)
	require.NoError(t, err)
	store.setConfig(marked) // Only the forthcoming typed logout transaction publishes this marker in production.
	selected := store.RuntimeSnapshot().AgentModelState()
	require.NoError(t, store.SetConfigField(ScopeWorkspace, "options.disable_auto_summarize", true))
	require.ErrorIs(t, store.RuntimeSnapshot().AuthenticationRevocation("fixture"), ErrAuthenticationRevoked)
	require.Equal(t, selected, store.RuntimeSnapshot().AgentModelState())
	require.NoError(t, store.SetConfigField(ScopeWorkspace, "providers.fixture.api_key", "synthetic-replacement"))
	require.NoError(t, store.RuntimeSnapshot().AuthenticationRevocation("fixture"))
	require.Empty(t, store.Config().authenticationRevocations)
	require.NoError(t, store.SetConfigField(ScopeWorkspace, "providers.fixture.api_key", ""))
	require.NoError(t, store.RuntimeSnapshot().AuthenticationRevocation("fixture"), "a retired marker must not reappear after later generic edits")
}

func TestAuthenticationRevocationDoesNotFollowReplacementPresetOwner(t *testing.T) {
	first := ProviderPresetReference{ID: "fixture.preset", Version: "1.0.0", Digest: strings.Repeat("a", 64)}
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{"fixture": {
		ID: "fixture", Type: catalog.TypeOpenAICompat, Preset: &first,
		Owner: &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat},
	}})}
	store := NewTestStoreWithProviderGeneration(cfg, map[string]ProviderPresetReference{"fixture": first})
	owner, ok := store.RuntimeSnapshot().ProviderOwner("fixture")
	require.True(t, ok)
	marked, err := cfg.WithAuthenticationRevocation(owner)
	require.NoError(t, err)
	second := first
	second.Version, second.Digest = "2.0.0", strings.Repeat("b", 64)
	replaced := marked.cloneForWrite()
	provider, _ := replaced.Providers.Get("fixture")
	provider.Preset = &second
	replaced.Providers.Set("fixture", provider)
	replacement := NewTestStoreWithProviderGeneration(replaced, map[string]ProviderPresetReference{"fixture": second})
	replacementOwner, ok := replacement.RuntimeSnapshot().ProviderOwner("fixture")
	require.True(t, ok)
	require.NotEqual(t, owner, replacementOwner)
	require.NoError(t, replacement.RuntimeSnapshot().AuthenticationRevocation("fixture"))
	_, err = replaced.WithAuthenticationRevocation(owner)
	require.ErrorContains(t, err, "exact owner")
	require.Empty(t, replaced.cloneForWrite().authenticationRevocations)
}
