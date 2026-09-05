package config

import (
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

// TestCloneForWrite_Isolation verifies that mutating a clone never reaches
// back into the original Config. This is the contract the store's
// copy-on-write mutators depend on for race-free publishing.
func TestCloneForWrite_Isolation(t *testing.T) {
	t.Parallel()

	temperature := 0.2
	autoDiscover := true
	orig := &Config{
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "openai", Model: "gpt-4"},
		},
		RecentModels: map[SelectedModelType][]SelectedModel{
			SelectedModelTypeLarge: {{Provider: "openai", Model: "gpt-4"}},
		},
		MCP: MCPs{"a": {}},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"openai": {
				ID: "openai", APIKey: "original", OAuthToken: &oauth.Token{AccessToken: "original", Client: &oauth.OAuthClient{ClientID: "original"}},
				Owner:           &ProviderOwnerReference{Type: ProviderOwnerPlugin, Construction: providerregistry.ConstructionGenericJSON},
				Plugin:          &ProviderPluginReference{ID: "plugin.original", Version: "1.0.0"},
				Preset:          &ProviderPresetReference{ID: "preset.original", Version: "1.0.0", Digest: "original"},
				ExtraHeaders:    map[string]string{"X-Test": "original"},
				ExtraBody:       map[string]any{"nested": map[string]any{"enabled": true}, "typed": map[string][]string{"values": {"original"}}},
				ProviderOptions: map[string]any{"values": []string{"original"}},
				Configuration:   map[string]any{"nested": []map[string]string{{"value": "original"}}},
				ExtraParams:     map[string]string{"original": "true"}, AutoDiscoverModels: &autoDiscover,
				Models: []catalog.Model{{ID: "gpt-4", ReasoningLevels: []string{"high"}, Options: catalog.ModelOptions{Temperature: &temperature, ProviderOptions: map[string]any{"nested": map[string]string{"value": "original"}}}}},
			},
		}),
		Options: &Options{
			TUI: &TUIOptions{CompactMode: false},
		},
		transportProviderOwners: map[string]providerregistry.RegistrationOwner{"openai": {ProviderID: "openai", ManifestVersion: "1.0.0"}},
		explicitModels:          map[SelectedModelType]bool{SelectedModelTypeLarge: true},
	}

	clone := orig.cloneForWrite()

	// Mutate every field the typed mutators touch.
	clone.Models[SelectedModelTypeLarge] = SelectedModel{Provider: "anthropic", Model: "claude"}
	clone.transportProviderOwners["openai"] = providerregistry.RegistrationOwner{ProviderID: "openai", ManifestVersion: "2.0.0"}
	clone.explicitModels[SelectedModelTypeSmall] = true
	clone.RecentModels[SelectedModelTypeLarge] = []SelectedModel{{Provider: "anthropic", Model: "claude"}}
	clone.MCP["b"] = MCPConfig{}
	clone.Options.TUI.CompactMode = true
	enabled := true
	clone.Options.TUI.Transparent = &enabled
	provider, ok := clone.Providers.Get("openai")
	require.True(t, ok)
	provider.APIKey = "changed"
	provider.OAuthToken.AccessToken = "changed"
	provider.OAuthToken.Client.ClientID = "changed"
	provider.Owner.Construction = providerregistry.ConstructionOpenAICompat
	provider.Plugin.ID = "plugin.changed"
	provider.Preset.Digest = "changed"
	provider.ExtraHeaders["X-Test"] = "changed"
	provider.ExtraBody["nested"].(map[string]any)["enabled"] = false
	provider.ExtraBody["typed"].(map[string][]string)["values"][0] = "changed"
	provider.ProviderOptions["values"].([]string)[0] = "changed"
	provider.Configuration["nested"].([]map[string]string)[0]["value"] = "changed"
	provider.ExtraParams["changed"] = "true"
	*provider.AutoDiscoverModels = false
	provider.Models[0].ReasoningLevels[0] = "low"
	*provider.Models[0].Options.Temperature = 0.9
	provider.Models[0].Options.ProviderOptions["nested"].(map[string]string)["value"] = "changed"
	clone.Providers.Set("openai", provider)

	// The original must be untouched.
	require.Equal(t, "openai", orig.Models[SelectedModelTypeLarge].Provider, "Models leaked")
	require.Equal(t, "1.0.0", orig.transportProviderOwners["openai"].ManifestVersion, "transportProviderOwners leaked")
	require.False(t, orig.explicitModels[SelectedModelTypeSmall], "explicitModels leaked")
	require.Equal(t, "openai", orig.RecentModels[SelectedModelTypeLarge][0].Provider, "RecentModels leaked")
	require.NotContains(t, orig.MCP, "b", "MCP leaked")
	require.False(t, orig.Options.TUI.CompactMode, "Options.TUI.CompactMode leaked")
	require.Nil(t, orig.Options.TUI.Transparent, "Options.TUI.Transparent leaked")
	originalProvider, ok := orig.Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "original", originalProvider.APIKey)
	require.Equal(t, "original", originalProvider.OAuthToken.AccessToken)
	require.Equal(t, "original", originalProvider.OAuthToken.Client.ClientID)
	require.Equal(t, providerregistry.ConstructionGenericJSON, originalProvider.Owner.Construction)
	require.Equal(t, "plugin.original", originalProvider.Plugin.ID)
	require.Equal(t, "original", originalProvider.Preset.Digest)
	require.Equal(t, "original", originalProvider.ExtraHeaders["X-Test"])
	require.True(t, originalProvider.ExtraBody["nested"].(map[string]any)["enabled"].(bool))
	require.Equal(t, "original", originalProvider.ExtraBody["typed"].(map[string][]string)["values"][0])
	require.Equal(t, "original", originalProvider.ProviderOptions["values"].([]string)[0])
	require.Equal(t, "original", originalProvider.Configuration["nested"].([]map[string]string)[0]["value"])
	require.NotContains(t, originalProvider.ExtraParams, "changed")
	require.True(t, *originalProvider.AutoDiscoverModels)
	require.Equal(t, "high", originalProvider.Models[0].ReasoningLevels[0])
	require.Equal(t, 0.2, *originalProvider.Models[0].Options.Temperature)
	require.Equal(t, "original", originalProvider.Models[0].Options.ProviderOptions["nested"].(map[string]string)["value"])
}
