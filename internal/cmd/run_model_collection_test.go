package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestRunModelFlagsApplyBeforeInitialClientCollection(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("CRUX_PROVIDER_PROFILE", "integrated")
	for _, key := range []string{"HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	root, dataDir := t.TempDir(), t.TempDir()
	data := []byte(`{"providers":{"codex":{"api_key":"synthetic-unselected-token","owner":{"type":"core","construction":"integrated-codex"},"models":[{"id":"fixture","name":"Fixture"}]},"chosen":{"type":"openai-compat","api_key":"synthetic-chosen","base_url":"https://chosen.invalid/v1","models":[{"id":"flag","name":"Flag"},{"id":"tiny","name":"Tiny"}]}},"models":{"large":{"provider":"codex","model":"fixture"},"small":{"provider":"codex","model":"fixture"}}}`)
	path := filepath.Join(dataDir, "crux.json")
	require.NoError(t, os.WriteFile(path, data, 0600))
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, accounts.Entry{ID: "other", AccessToken: "synthetic-unselected-active"}))
	_, err := collectRemoteProviderState(t.Context(), root, dataDir, false, 1)
	require.ErrorContains(t, err, "selected client account changed")
	command := &cobra.Command{Use: "run"}
	command.SetContext(t.Context())
	command.Flags().String("model", "chosen/flag", "")
	command.Flags().String("small-model", "chosen/tiny", "")
	proposal, err := collectRemoteProviderStatePrepared(t.Context(), nil, root, dataDir, false, 1, prepareRunModelOverrides(command))
	require.NoError(t, err)
	require.Equal(t, config.SelectedModel{Provider: "chosen", Model: "flag"}, proposal.Models[config.SelectedModelTypeLarge])
	require.Equal(t, config.SelectedModel{Provider: "chosen", Model: "tiny"}, proposal.Models[config.SelectedModelTypeSmall])
	require.Len(t, proposal.Providers, 1)
	require.Len(t, proposal.Credentials, 1)
	encoded, err := json.Marshal(proposal)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "synthetic-unselected")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, after, "run flags must never persist model choices")
	require.NoError(t, command.Flags().Set("model", "chosen/missing"))
	proposal, err = collectRemoteProviderStatePrepared(t.Context(), nil, root, dataDir, false, 1, prepareRunModelOverrides(command))
	require.Error(t, err)
	require.Nil(t, proposal)
	after, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, after)
	require.NoError(t, command.Flags().Set("model", ""))
	proposal, err = collectRemoteProviderStatePrepared(t.Context(), nil, root, dataDir, false, 1, prepareRunModelOverrides(command))
	require.ErrorContains(t, err, "--model requires a model")
	require.Nil(t, proposal)
	command.Use = "other-command"
	require.Nil(t, prepareRunModelOverrides(command))
}
