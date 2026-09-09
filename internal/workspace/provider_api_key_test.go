package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceCheckedAPIKeyTLSReceiptAndInference(t *testing.T) {
	for _, mode := range []string{"acknowledged", "lost-response", "rejected", "shadowed"} {
		t.Run(mode, func(t *testing.T) {
			f := newClientAuthenticationFixture(t, true)
			if mode != "shadowed" {
				projectPath := filepath.Join(f.root, "crux.json")
				projectData, readErr := os.ReadFile(projectPath)
				require.NoError(t, readErr)
				var project map[string]any
				require.NoError(t, json.Unmarshal(projectData, &project))
				other := project["providers"].(map[string]any)["other"].(map[string]any)
				originalSource := other["api_key"]
				delete(other, "api_key")
				projectData, readErr = json.Marshal(project)
				require.NoError(t, readErr)
				require.NoError(t, os.WriteFile(projectPath, projectData, 0600))
				require.NoError(t, f.w.mutateClientAuthority(t.Context(), func(store *config.ConfigStore) error {
					return store.SetConfigField(config.ScopeGlobal, "providers.other.api_key", originalSource)
				}))
			}
			owner, ok := f.store.RuntimeSnapshot().ProviderOwner("other")
			require.True(t, ok)
			_, err := f.w.UpdatePreferredModel(config.ScopeGlobal, config.SelectedModelTypeLarge, config.SelectedModel{Provider: "other", Model: "other"}, owner)
			require.NoError(t, err)
			require.NoError(t, f.w.InitCoderAgentNonInteractive(t.Context()))
			receiver, err := f.s.Backend().GetWorkspace(f.w.workspaceID())
			require.NoError(t, err)
			counter := filepath.Join(f.root, "checked-key-evaluations")
			literal := "checked-$(touch " + f.marker + ")-$AUTH_LITERAL"
			source := fmt.Sprintf("$(printf x >> '%s'; printf '%%s' '%s')", counter, strings.ReplaceAll(literal, "'", "'\\''"))
			beforeAccounts, err := os.ReadFile(f.accountsPath)
			require.NoError(t, err)
			models := f.store.RuntimeSnapshot().AgentModelState()
			status, err := f.w.ProviderAuthentication(t.Context())
			require.NoError(t, err)
			request := providerauth.APIKeyCheckRequest{CheckID: strings.Repeat("a", 32), Target: providerauth.Target{WorkspaceID: status.WorkspaceID, Generation: status.Generation, Owner: providerauth.PublicOwner(owner)}, CredentialID: "provider.api_key", Source: source}
			checked, err := f.w.CheckProviderAPIKey(t.Context(), request)
			require.NoError(t, err)
			require.NotNil(t, checked.CheckedTarget)
			again, err := f.w.CheckProviderAPIKey(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, checked, again)
			save := providerauth.APIKeySaveRequest{OperationID: strings.Repeat("b", 32), CheckID: request.CheckID, Target: *checked.CheckedTarget}
			baseline := f.puts.Load()
			beforeConfig, readErr := os.ReadFile(f.path)
			require.NoError(t, readErr)
			if mode == "lost-response" {
				f.putMode.Store(2)
				f.getMode.Store(1)
			}
			if mode == "rejected" {
				f.putMode.Store(1)
			}
			outcome, err := f.w.SaveCheckedProviderAPIKey(t.Context(), save)
			if mode == "shadowed" {
				require.Error(t, err)
				require.Nil(t, outcome.Change)
				require.Equal(t, providerauth.MutationProgress{}, outcome.Progress)
				require.Equal(t, baseline, f.puts.Load())
				afterConfig, readErr := os.ReadFile(f.path)
				require.NoError(t, readErr)
				require.Equal(t, beforeConfig, afterConfig)
				afterAccounts, readErr := os.ReadFile(f.accountsPath)
				require.NoError(t, readErr)
				require.Equal(t, beforeAccounts, afterAccounts)
				evaluations, readErr := os.ReadFile(counter)
				require.NoError(t, readErr)
				require.Equal(t, "x", string(evaluations))
				require.NoFileExists(t, f.marker)
				return
			}
			if mode == "acknowledged" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, outcome.ValidateAPIKeySave(save))
			require.NotNil(t, outcome.Change)
			require.Equal(t, providerauth.MutationProgress{ConfigSaved: true, RuntimePublished: true}, outcome.Progress)
			require.Equal(t, baseline+1, f.puts.Load())
			require.Equal(t, models, f.store.RuntimeSnapshot().AgentModelState())
			f.getMode.Store(0)
			f.putMode.Store(0)
			replay, err := f.w.SaveCheckedProviderAPIKey(t.Context(), save)
			require.Equal(t, outcome, replay)
			require.Equal(t, baseline+1, f.puts.Load(), "receipt retry must never repeat PUT")
			if mode == "rejected" {
				require.Error(t, err)
				_, err = f.w.CheckProviderAPIKey(t.Context(), request)
				require.Error(t, err, "unacknowledged key save must block subsequent check")
			} else {
				require.NoError(t, err)
				require.Equal(t, models, receiver.Cfg.RuntimeSnapshot().AgentModelState())
				_, err = receiver.App.CurrentAgentCoordinator().Model().Model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("checked key receiver")}})
				require.NoError(t, err)
				observed := f.observed()
				require.Equal(t, "Bearer "+literal, observed[len(observed)-1])
			}
			afterAccounts, err := os.ReadFile(f.accountsPath)
			require.NoError(t, err)
			require.Equal(t, beforeAccounts, afterAccounts)
			evaluations, err := os.ReadFile(counter)
			require.NoError(t, err)
			require.Equal(t, "x", string(evaluations))
			require.NoFileExists(t, f.marker)
		})
	}
}
