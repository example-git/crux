package config

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/csync"
	"github.com/stretchr/testify/require"
)

func TestRuntimeControlAgentModelGenerationUsesExactJSONNumbers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	registration := ownerTestRegistration("owner-test", "numeric.fixture")
	provider := ownerMutationTestProvider(registration)
	store := NewTestStoreWithRegistrations(&Config{Options: &Options{}, Providers: csync.NewMapFrom(map[string]ProviderConfig{provider.ID: provider}), Models: map[SelectedModelType]SelectedModel{
		SelectedModelTypeLarge: {Provider: provider.ID, Model: "old-model", ProviderOptions: map[string]any{"vendor.count": json.Number("9007199254740993"), "nested": []any{json.Number("0"), json.Number("1000")}}},
		SelectedModelTypeSmall: {Provider: provider.ID, Model: "old-model"},
	}}, registration)
	snapshot := store.RuntimeSnapshot()
	for _, mode := range []string{"same", "equivalent encodings", "rounded", "changed sibling", "changed owner"} {
		t.Run(mode, func(t *testing.T) {
			expected := snapshot.AgentModelState()
			switch mode {
			case "equivalent encodings":
				expected.Large.Model.ProviderOptions["nested"] = []any{float64(0), json.Number("1e3")}
			case "rounded":
				expected.Large.Model.ProviderOptions["vendor.count"] = float64(9007199254740993)
			case "changed sibling":
				expected.Small.Model.Think = true
			case "changed owner":
				expected.Large.Owner.ManifestVersion = "replacement"
			}
			err := snapshot.ValidateAgentModelState(expected)
			if mode == "same" || mode == "equivalent encodings" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "generation changed")
			}
		})
	}
}
