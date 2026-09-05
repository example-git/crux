package dialog

import (
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/stretchr/testify/require"
)

func TestModelProviderSelectable(t *testing.T) {
	cfg := &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
		"available": {
			ID: "available", Models: []catalog.Model{{ID: "available-model"}},
		},
		"disabled": {
			ID: "disabled", Disable: true, Models: []catalog.Model{{ID: "disabled-model"}},
		},
		"unavailable": {
			ID: "unavailable", Models: []catalog.Model{{ID: "unavailable-model"}},
			Plugin: &config.ProviderPluginReference{ID: "missing.plugin", Version: "1"},
		},
	})}

	require.True(t, modelProviderSelectable(cfg, "unconfigured"))
	require.True(t, modelProviderSelectable(cfg, "available"))
	require.False(t, modelProviderSelectable(cfg, "disabled"))
	require.False(t, modelProviderSelectable(cfg, "unavailable"))
}
