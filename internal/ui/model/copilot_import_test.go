package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type importingTestWorkspace struct {
	*testWorkspace
	importToken func(context.Context, providerregistry.RegistrationOwner) (bool, error)
}

func (w *importingTestWorkspace) ImportCopilot(ctx context.Context, owner providerregistry.RegistrationOwner) (bool, error) {
	return w.importToken(ctx, owner)
}

func (w *importingTestWorkspace) PermissionSkipRequests() bool                     { return false }
func (w *importingTestWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo { return nil }

func newImportTestUI(t *testing.T) (*UI, *importingTestWorkspace, dialog.ActionSelectModel, func() *config.Config) {
	t.Helper()
	var registration providerregistry.Registration
	for _, value := range providerregistry.Integrated() {
		if value.ProviderID == "copilot" {
			registration = value
			break
		}
	}
	require.NotEmpty(t, registration.ProviderID)
	configured := func() *config.Config {
		providers := csync.NewMap[string, config.ProviderConfig]()
		providers.Set("copilot", config.ProviderConfig{ID: "copilot", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCopilot}})
		cfg := &config.Config{Providers: providers, Models: map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeSmall: {Provider: "copilot", Model: "fixture"}}, Options: &config.Options{}}
		return config.NewTestStoreWithRegistrations(cfg, registration).Config()
	}
	cfg := configured()
	provider, _ := cfg.Providers.Get("copilot")
	cfg.Providers = csync.NewMap[string, config.ProviderConfig]()
	w := &importingTestWorkspace{testWorkspace: &testWorkspace{cfg: cfg}}
	ui := newTestUI()
	ui.com.Workspace = w
	ui.dialog = dialog.NewOverlay()
	t.Cleanup(func() { ui.dialog.CloseDialog(dialog.LoginID) })
	ui.header = newHeader(ui.com)
	ui.themeKey = styles.ThemeKeyForProvider("copilot")
	ui.brand = ui.brandForProvider("copilot")
	action := dialog.ActionSelectModel{Provider: provider.ToProvider(), Model: config.SelectedModel{Provider: "copilot", Model: "fixture"}, ModelType: config.SelectedModelTypeLarge, ProviderOwner: registration.Owner(), ProviderOwnerSet: true}
	return ui, w, action, configured
}

func TestCopilotImportCompletesThroughUIMessage(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprintf("public-owner-only=%t", remote), func(t *testing.T) {
			ui, w, action, configured := newImportTestUI(t)
			public := func(cfg *config.Config) *config.Config {
				if !remote {
					return cfg
				}
				// Match a received config: no registry or executable capability,
				// with exact owners bound from the public provider surfaces.
				result := &config.Config{Providers: cfg.Providers, Models: cfg.Models, Options: cfg.Options}
				require.NoError(t, result.BindProviderSurfaceOwners([]providerregistry.Surface{{ID: "copilot", Owner: &action.ProviderOwner}}))
				_, registered := result.ProviderRegistration("copilot")
				require.False(t, registered)
				return result
			}
			w.cfg = public(w.cfg)
			ui.brand = ui.brandForProvider("copilot")
			called := false
			w.importToken = func(_ context.Context, owner providerregistry.RegistrationOwner) (bool, error) {
				called = true
				require.Equal(t, action.ProviderOwner, owner)
				w.cfg = public(configured())
				return true, nil
			}
			command := ui.handleSelectModel(action)
			require.NotNil(t, command)
			require.False(t, called, "Update must not perform token exchange")
			message, ok := command().(copilotImportDoneMsg)
			require.True(t, ok)
			require.NoError(t, message.err)
			require.True(t, called)
			require.Zero(t, w.preferredModelCalls, "the command must not mutate UI selection")
			_, command = ui.Update(message)
			require.Zero(t, w.preferredModelCalls, "Update must only queue model persistence")
			messages := collectCommandMessages(command)
			require.Equal(t, 1, w.preferredModelCalls)
			var applied bool
			for _, message := range messages {
				if result, ok := message.(modelSelectionCompletedMsg); ok {
					require.Equal(t, "copilot", result.providerID)
					applied = true
				}
			}
			require.True(t, applied, "selection must rebuild the agent and report completion")
			require.Equal(t, 1, w.updateAgentCalls)
		})
	}
}

func TestCopilotImportFailureDoesNotSelectOrRetry(t *testing.T) {
	ui, w, action, _ := newImportTestUI(t)
	called := 0
	w.importToken = func(context.Context, providerregistry.RegistrationOwner) (bool, error) {
		called++
		return false, errors.New("synthetic import failure")
	}
	message, ok := ui.handleSelectModel(action)().(copilotImportDoneMsg)
	require.True(t, ok)
	require.ErrorContains(t, message.err, "synthetic import failure")
	_, command := ui.Update(message)
	require.NotNil(t, command, "import errors must be reported")
	requireCommandError(t, collectCommandMessages(command), "synthetic import failure")
	require.Equal(t, 1, called)
	require.Zero(t, w.preferredModelCalls)
}

func TestNewModelSelectionCancelsAndDiscardsOlderImport(t *testing.T) {
	ui, w, action, _ := newImportTestUI(t)
	started := make(chan context.Context, 1)
	w.importToken = func(ctx context.Context, _ providerregistry.RegistrationOwner) (bool, error) {
		started <- ctx
		<-ctx.Done()
		return false, ctx.Err()
	}
	command := ui.handleSelectModel(action)
	done := make(chan tea.Msg, 1)
	go func() { done <- command() }()
	var ctx context.Context
	select {
	case ctx = <-started:
	case <-time.After(time.Second):
		t.Fatal("import command did not start")
	}
	next := ui.handleSelectModel(action)
	require.NotNil(t, next)
	defer ui.cancelCopilotImport()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("new selection did not cancel old import")
	}
	select {
	case message := <-done:
		_, _ = ui.Update(message)
	case <-time.After(time.Second):
		t.Fatal("canceled import did not return")
	}
	require.Zero(t, w.preferredModelCalls)
}

func TestCopilotImportSelectionBranches(t *testing.T) {
	for _, mode := range []string{"no-source", "configured", "reauthenticate", "owner-changed"} {
		t.Run(mode, func(t *testing.T) {
			ui, w, action, configured := newImportTestUI(t)
			calls := 0
			w.importToken = func(context.Context, providerregistry.RegistrationOwner) (bool, error) {
				calls++
				return false, nil
			}
			if mode == "configured" {
				w.cfg = configured()
			}
			action.ReAuthenticate = mode == "reauthenticate"
			command := ui.handleSelectModel(action)
			require.NotNil(t, command)
			if mode == "configured" {
				require.Zero(t, w.preferredModelCalls)
				collectCommandMessages(command)
				require.Equal(t, 1, w.preferredModelCalls)
				require.Zero(t, calls)
				require.Nil(t, ui.cancelCopilotImport)
				return
			}
			if mode == "reauthenticate" {
				require.Zero(t, calls)
				require.Nil(t, ui.cancelCopilotImport)
				require.True(t, ui.dialog.ContainsDialog(dialog.LoginID))
				return
			}
			message, ok := command().(copilotImportDoneMsg)
			require.True(t, ok)
			require.NoError(t, message.err)
			if mode == "owner-changed" {
				// Public owner metadata changes while the command is in flight.
				nextOwner := action.ProviderOwner
				nextOwner.OAuthFlowID = "replacement-flow"
				w.cfg = &config.Config{Providers: csync.NewMap[string, config.ProviderConfig](), Options: &config.Options{}}
				require.NoError(t, w.cfg.BindProviderSurfaceOwners([]providerregistry.Surface{{ID: "copilot", Owner: &nextOwner}}))
			}
			_, result := ui.Update(message)
			require.Equal(t, 1, calls)
			require.Zero(t, w.preferredModelCalls)
			if mode == "owner-changed" {
				requireCommandError(t, collectCommandMessages(result), "provider owner changed")
				require.False(t, ui.dialog.ContainsDialog(dialog.LoginID))
			} else {
				require.True(t, ui.dialog.ContainsDialog(dialog.LoginID), "no source must continue to interactive login")
				require.False(t, ui.dialog.ContainsDialog(dialog.APIKeyInputID))
			}
		})
	}
}

func TestCopilotImportRetainsWorkspaceAndSelectionValues(t *testing.T) {
	ui, original, action, configured := newImportTestUI(t)
	original.importToken = func(context.Context, providerregistry.RegistrationOwner) (bool, error) {
		original.cfg = configured()
		return true, nil
	}
	temperature := 0.2
	action.Model.Temperature = &temperature
	action.Model.ProviderOptions = map[string]any{"nested": map[string]any{"value": "original"}}
	command := ui.handleSelectModel(action)
	temperature = 0.8
	action.Model.ProviderOptions["nested"].(map[string]any)["value"] = "replacement"
	completed, ok := command().(copilotImportDoneMsg)
	require.True(t, ok)
	require.NoError(t, completed.err)
	require.Same(t, original, completed.workspace)
	require.Equal(t, 0.2, *completed.selection.Model.Temperature)
	require.Equal(t, "original", completed.selection.Model.ProviderOptions["nested"].(map[string]any)["value"])
	replacement := &importingTestWorkspace{testWorkspace: &testWorkspace{cfg: configured()}}
	ui.com.Workspace = replacement
	_, _ = ui.Update(completed)
	require.Empty(t, ui.modelSelectionLanes)
	require.Zero(t, replacement.preferredModelCalls)
	require.Zero(t, original.preferredModelCalls)
}
