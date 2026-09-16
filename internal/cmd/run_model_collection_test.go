package cmd

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestRunModelFlagsApplyBeforeInitialClientCollection(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("CRUX_PROVIDER_PROFILE", "plugin-compat")
	for _, key := range []string{"HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	require.NoError(t, registrytest.Install(t.Context(), os.Getenv("CRUX_GLOBAL_DATA"), os.Getenv("CRUX_CACHE_DIR"), *registrytest.Provider("codex").Manifest))
	data := []byte(`{"providers":{"codex":{"api_key":"synthetic-unselected-token","plugin":{"id":"test.codex","version":"1.1.0"},"owner":{"type":"plugin","construction":"integrated-codex","compatibility_adapter":"integrated-codex"},"models":[{"id":"fixture","name":"Fixture"}]},"chosen":{"owner":{"type":"custom","construction":"openai-compat"},"type":"openai-compat","api_key":"synthetic-chosen","base_url":"https://chosen.invalid/v1","models":[{"id":"flag","name":"Flag"},{"id":"tiny","name":"Tiny"}]}},"models":{"large":{"provider":"codex","model":"fixture"},"small":{"provider":"codex","model":"fixture"}}}`)
	path := config.GlobalConfigData()
	require.NoError(t, os.WriteFile(path, data, 0o600))
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, accounts.Entry{ID: "other", AccessToken: "synthetic-unselected-active"}))
	initial, err := collectRemoteProviderState(t.Context(), false, 1)
	require.NoError(t, err)
	require.Len(t, initial.Credentials, 1)
	require.Equal(t, "synthetic-unselected-active", initial.Credentials[0].Account.AccessToken)

	// The fixture deliberately leaves providers.codex.api_key on disk out of
	// sync with the accounts store's active entry for codex, so the initial
	// collection above intentionally heals that mismatch onto disk (see the
	// NOTE comments in ConfigStore.rebindSelectedRemoteAccountsLocked,
	// reloadFromDiskWithCredentialCaptureLocked, and loadWithEnvironment):
	// without persisting the heal, a later strict refresh would reject the
	// stale disk credential as an unexplained change. That healed disk state
	// -- not the pre-heal fixture bytes -- is the correct baseline for the
	// "run flags never persist a model choice" assertions below.
	healed, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEqual(t, data, healed, "expected the mismatched codex credential to be healed onto disk")
	healedProbe := readDiskModelProbe(t, healed)
	require.Equal(t, "synthetic-unselected-active", healedProbe.Providers["codex"].APIKey, "codex credential should be healed to the active account")
	require.Equal(t, "codex", healedProbe.Models.Large.Provider, "healing a credential must not change the selected model")
	require.Equal(t, "codex", healedProbe.Models.Small.Provider, "healing a credential must not change the selected model")

	command := &cobra.Command{Use: "run"}
	command.SetContext(t.Context())
	command.Flags().String("model", "chosen/flag", "")
	command.Flags().String("small-model", "chosen/tiny", "")
	proposal, err := collectRemoteProviderStatePrepared(t.Context(), nil, false, 1, prepareRunModelOverrides(command))
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
	require.Equal(t, healed, after, "run flags must never persist model choices, only the earlier credential heal may remain")
	afterProbe := readDiskModelProbe(t, after)
	require.Equal(t, "codex", afterProbe.Models.Large.Provider, "run flags must never persist a model choice")
	require.Equal(t, "codex", afterProbe.Models.Small.Provider, "run flags must never persist a model choice")
	require.NoError(t, command.Flags().Set("model", "chosen/missing"))
	proposal, err = collectRemoteProviderStatePrepared(t.Context(), nil, false, 1, prepareRunModelOverrides(command))
	require.Error(t, err)
	require.Nil(t, proposal)
	after, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, healed, after)
	require.NoError(t, command.Flags().Set("model", ""))
	proposal, err = collectRemoteProviderStatePrepared(t.Context(), nil, false, 1, prepareRunModelOverrides(command))
	require.ErrorContains(t, err, "--model requires a model")
	require.Nil(t, proposal)
	command.Use = "other-command"
	require.Nil(t, prepareRunModelOverrides(command))
}

// diskModelProbe extracts just the fields this test needs to verify from raw
// global config bytes: the per-provider api_key (to observe the intentional
// credential-healing write) and the selected model provider names (to prove
// run flags never persist a model choice to disk).
type diskModelProbe struct {
	Providers map[string]struct {
		APIKey string `json:"api_key"`
	} `json:"providers"`
	Models struct {
		Large struct {
			Provider string `json:"provider"`
		} `json:"large"`
		Small struct {
			Provider string `json:"provider"`
		} `json:"small"`
	} `json:"models"`
}

func readDiskModelProbe(t *testing.T, data []byte) diskModelProbe {
	t.Helper()
	var probe diskModelProbe
	require.NoError(t, json.Unmarshal(data, &probe))
	return probe
}
