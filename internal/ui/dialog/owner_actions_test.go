package dialog

import (
	"context"
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type ownerActionWorkspace struct {
	workspace.Workspace
	cfg               *config.Config
	fields            map[string]any
	apiKeyCredentials []config.ProviderAPIKeyCredential
}

func (w *ownerActionWorkspace) Config() *config.Config {
	return w.cfg
}

func (w *ownerActionWorkspace) SetConfigField(_ config.Scope, key string, value any) error {
	w.fields[key] = value
	return nil
}

func (w *ownerActionWorkspace) SetProviderAPIKey(_ config.Scope, _ string, value any) error {
	credential, ok := value.(config.ProviderAPIKeyCredential)
	if !ok {
		return fmt.Errorf("credential is not owner-bound")
	}
	w.apiKeyCredentials = append(w.apiKeyCredentials, credential)
	return nil
}

func ownerActionRegistration(providerID, pluginID string, withOAuth bool) providerregistry.Registration {
	registration := providerregistry.Registration{
		ProviderID:   providerID,
		Name:         "Synthetic Provider",
		Construction: providerregistry.ConstructionGenericJSON,
		Manifest:     &manifest.Manifest{ID: pluginID, Version: "1.0.0"},
	}
	if withOAuth {
		registration.AccountNamespace = providerID + "-account"
		registration.OAuth = &providerregistry.OAuthCapability{
			Adapter: providerregistry.LoginBrowser,
			FlowID:  pluginID + ".flow",
			Authorize: func(context.Context, providerregistry.OpenURL, providerregistry.ReadCode) (*oauth.Token, error) {
				return &oauth.Token{AccessToken: "synthetic-access"}, nil
			},
		}
	}
	return registration
}

func ownerActionConfig(registration providerregistry.Registration) *config.Config {
	return ownerActionConfigFor(registration, registration)
}

func ownerActionConfigFor(persisted, active providerregistry.Registration) *config.Config {
	model := catalog.Model{ID: "shared-model", Name: "Shared Model"}
	provider := config.ProviderConfig{
		ID:     persisted.ProviderID,
		Name:   persisted.Name,
		Models: []catalog.Model{model},
		Owner: &config.ProviderOwnerReference{
			Type:                 config.ProviderOwnerPlugin,
			Construction:         persisted.Construction,
			CompatibilityAdapter: persisted.CompatibilityAdapter,
		},
		Plugin: &config.ProviderPluginReference{
			ID:      persisted.Manifest.ID,
			Version: persisted.Manifest.Version,
		},
	}
	if persisted.OAuth != nil {
		provider.OAuthToken = &oauth.Token{AccessToken: "synthetic-configured-access"}
	}
	cfg := &config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{persisted.ProviderID: provider}),
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: persisted.ProviderID, Model: model.ID},
		},
	}
	return config.NewTestStoreWithRegistrations(cfg, active).RuntimeSnapshot().Config()
}

func ownerActionSelection(t *testing.T, cfg *config.Config, providerID string) ActionSelectModel {
	t.Helper()
	provider, ok := cfg.Providers.Get(providerID)
	require.True(t, ok)
	owner, ok := cfg.ProviderOwner(providerID)
	require.True(t, ok)
	return ActionSelectModel{
		Provider:         provider.ToProvider(),
		Model:            config.SelectedModel{Provider: providerID, Model: "shared-model"},
		ModelType:        config.SelectedModelTypeLarge,
		ProviderOwner:    owner,
		ProviderOwnerSet: true,
	}
}

func TestProviderDialogRejectsStaleSameIDOwnerToggle(t *testing.T) {
	const providerID = "same-id-provider"
	ownerA := ownerActionRegistration(providerID, "plugin.owner-a", false)
	ownerB := ownerActionRegistration(providerID, "plugin.owner-b", false)
	workspace := &ownerActionWorkspace{cfg: ownerActionConfig(ownerA), fields: make(map[string]any)}
	theme := styles.ThemeForProvider(providerID)
	dialog := NewProviders(&common.Common{Workspace: workspace, Styles: &theme})
	require.Len(t, dialog.items, 1)
	require.Equal(t, ownerA.Owner(), dialog.items[0].owner)

	workspace.cfg = ownerActionConfig(ownerB)
	action, ok := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionCmd)
	require.True(t, ok)
	require.Empty(t, workspace.fields)
	message, ok := action.Cmd().(util.InfoMsg)
	require.True(t, ok)
	require.Equal(t, util.InfoTypeError, message.Type)
	require.Contains(t, message.Msg, "provider owner changed")
}

func TestModelDialogRetainsOwnerAndRejectsSameIDReplacement(t *testing.T) {
	const providerID = "same-id-provider"
	ownerA := ownerActionRegistration(providerID, "plugin.owner-a", false)
	ownerB := ownerActionRegistration(providerID, "plugin.owner-b", false)
	workspace := &ownerActionWorkspace{cfg: ownerActionConfig(ownerA), fields: make(map[string]any)}
	theme := styles.ThemeForProvider(providerID)
	dialog, err := NewModels(&common.Common{Workspace: workspace, Styles: &theme}, false)
	require.NoError(t, err)

	workspace.cfg = ownerActionConfig(ownerB)
	action, ok := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionSelectModel)
	require.True(t, ok)
	require.Equal(t, ownerA.Owner(), action.ProviderOwner)
	require.True(t, action.ProviderOwnerSet)
	require.ErrorContains(t, action.ValidateProviderOwner(workspace.cfg), "owner changed")

	ownerless := action
	ownerless.ProviderOwnerSet = false
	require.ErrorContains(t, ownerless.ValidateProviderOwner(ownerActionConfig(ownerA)), "owner is missing")
	require.ErrorContains(t, action.ValidateProviderOwner(nil), "configuration not found")
}

func TestOAuthMenusRejectSameIDOwnerReplacement(t *testing.T) {
	const providerID = "same-id-provider"
	ownerA := ownerActionRegistration(providerID, "plugin.oauth-owner-a", true)
	ownerB := ownerActionRegistration(providerID, "plugin.oauth-owner-b", true)
	theme := styles.ThemeForProvider(providerID)

	workspace := &ownerActionWorkspace{cfg: ownerActionConfig(ownerB), fields: make(map[string]any)}
	common := &common.Common{Workspace: workspace, Styles: &theme}
	login, _ := NewLogin(common)
	logout := NewLogout(common)
	require.Len(t, login.list.FilteredItems(), 1)
	require.Len(t, logout.list.FilteredItems(), 1)

	workspace.cfg = ownerActionConfigFor(ownerA, ownerB)
	login, _ = NewLogin(common)
	logout = NewLogout(common)
	require.Empty(t, login.list.FilteredItems())
	require.Empty(t, logout.list.FilteredItems())
}

func TestModelAuthenticationContinuationsRetainExactOwner(t *testing.T) {
	const providerID = "same-id-provider"
	oauthOwnerA := ownerActionRegistration(providerID, "plugin.oauth-owner-a", true)
	oauthOwnerB := ownerActionRegistration(providerID, "plugin.oauth-owner-b", true)
	workspace := &ownerActionWorkspace{cfg: ownerActionConfig(oauthOwnerA), fields: make(map[string]any)}
	theme := styles.ThemeForProvider(providerID)
	common := &common.Common{Workspace: workspace, Styles: &theme}
	selection := ownerActionSelection(t, workspace.cfg, providerID)

	login, _, err := NewLoginForModel(common, selection)
	require.NoError(t, err)
	require.NotNil(t, login.continuation)
	require.Equal(t, selection, *login.continuation)

	workspace.cfg = ownerActionConfig(oauthOwnerB)
	_, _, err = NewLoginForModel(common, selection)
	require.ErrorContains(t, err, "owner changed")
}

func TestAPIKeyContinuationRetainsExactOwnerAndRejectsReplacement(t *testing.T) {
	const providerID = "same-id-provider"
	ownerA := ownerActionRegistration(providerID, "plugin.key-owner-a", false)
	ownerB := ownerActionRegistration(providerID, "plugin.key-owner-b", false)
	workspace := &ownerActionWorkspace{cfg: ownerActionConfig(ownerA), fields: make(map[string]any)}
	theme := styles.ThemeForProvider(providerID)
	selection := ownerActionSelection(t, workspace.cfg, providerID)
	input, _ := NewAPIKeyInput(&common.Common{Workspace: workspace, Styles: &theme}, false, selection)
	input.input.SetValue("synthetic-key")

	action, ok := input.saveKeyAndContinue().(ActionSelectModel)
	require.True(t, ok)
	require.Equal(t, selection.ProviderOwner, action.ProviderOwner)
	require.True(t, action.ProviderOwnerSet)
	require.Len(t, workspace.apiKeyCredentials, 1)
	require.Equal(t, selection.ProviderOwner, workspace.apiKeyCredentials[0].Owner)

	workspace.apiKeyCredentials = nil
	workspace.cfg = ownerActionConfig(ownerB)
	stale, ok := input.saveKeyAndContinue().(ActionCmd)
	require.True(t, ok)
	require.Empty(t, workspace.apiKeyCredentials)
	message, ok := stale.Cmd().(util.InfoMsg)
	require.True(t, ok)
	require.Contains(t, message.Msg, "provider owner changed")
}
