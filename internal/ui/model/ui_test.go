package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/agent/notify"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/imageattachment"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestProviderUsageUsesExactConfigurationToken(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	for _, test := range []struct {
		name            string
		providerID      string
		construction    providerregistry.Construction
		ownerType       config.ProviderOwnerType
		quotaCredential providerregistry.QuotaCredential
		wantToken       string
	}{
		{name: "claude plugin", providerID: "claude-ai", construction: providerregistry.ConstructionAnthropicMessages, ownerType: config.ProviderOwnerPlugin, wantToken: "configuration-access-token"},
		{name: "gemini plugin", providerID: "gemini-ag", construction: providerregistry.ConstructionGeminiContent, ownerType: config.ProviderOwnerPlugin, wantToken: "configuration-access-token"},
		{name: "codex core", providerID: "codex", construction: providerregistry.ConstructionCodex, ownerType: config.ProviderOwnerCore, wantToken: "configuration-access-token"},
		{name: "copilot core", providerID: "copilot", construction: providerregistry.ConstructionCopilot, ownerType: config.ProviderOwnerCore, quotaCredential: providerregistry.QuotaCredentialRefreshToken, wantToken: "configuration-refresh-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			accountNamespace := test.providerID + "-account"
			require.NoError(t, accounts.Save(t.Context(), accountNamespace, accounts.Entry{
				ID:          "stale-account",
				AccessToken: "stale-account-token",
			}))

			registration := providerregistry.Registration{
				ProviderID:       test.providerID,
				AccountNamespace: accountNamespace,
				Construction:     test.construction,
				OAuth:            &providerregistry.OAuthCapability{},
				QuotaCredential:  test.quotaCredential,
				Quota: func(_ context.Context, token string) (*oauthusage.Usage, error) {
					require.Equal(t, test.wantToken, token)
					return &oauthusage.Usage{Windows: []oauthusage.Window{{Name: "5h", Percent: 75}}}, nil
				},
			}
			provider := config.ProviderConfig{
				ID:     test.providerID,
				APIKey: "configuration-api-key",
				OAuthToken: &oauth.Token{
					AccessToken:  "configuration-access-token",
					RefreshToken: "configuration-refresh-token",
				},
				Owner: &config.ProviderOwnerReference{
					Type:         test.ownerType,
					Construction: registration.Construction,
				},
			}
			if test.ownerType == config.ProviderOwnerPlugin {
				registration.Manifest = &manifest.Manifest{ID: test.providerID, Version: "1.0.0"}
				provider.Plugin = &config.ProviderPluginReference{ID: test.providerID, Version: registration.Manifest.Version}
			}
			providers := csync.NewMap[string, config.ProviderConfig]()
			providers.Set(test.providerID, provider)
			cfg := &config.Config{Providers: providers}
			bound := config.NewTestStoreWithRegistrations(cfg, registration).RuntimeSnapshot().Config()
			ui := newTestUI()
			ui.com.Workspace = &testWorkspace{cfg: bound}

			cmd := ui.fetchProviderUsageFor(test.providerID)
			require.NotNil(t, cmd)
			msg, ok := cmd().(usageUpdatedMsg)
			require.True(t, ok)
			require.NotNil(t, msg.usage)
			require.Equal(t, test.providerID, msg.usage.ProviderID)
			_, _ = ui.Update(msg)
			require.Equal(t, msg.usage, ui.providerUsage)
			require.Contains(t, ui.usageBars(40, true), "25% left")
			stored, err := accounts.Active(t.Context(), accountNamespace)
			require.NoError(t, err)
			require.Equal(t, "stale-account-token", stored.AccessToken)
		})
	}
}

func TestProviderUsageRejectsConfigurationCredentialReplacement(t *testing.T) {
	const providerID = "copilot"
	workspace := &testWorkspace{}
	var replacement *config.Config
	registration := providerregistry.Registration{
		ProviderID:      providerID,
		Construction:    providerregistry.ConstructionCopilot,
		OAuth:           &providerregistry.OAuthCapability{},
		QuotaCredential: providerregistry.QuotaCredentialRefreshToken,
		Quota: func(_ context.Context, token string) (*oauthusage.Usage, error) {
			require.Equal(t, "github-token-before", token)
			workspace.cfg = replacement
			return &oauthusage.Usage{Windows: []oauthusage.Window{{Name: "premium_requests", Percent: 25}}}, nil
		},
	}
	configuration := func(refreshToken string) *config.Config {
		providers := csync.NewMap[string, config.ProviderConfig]()
		providers.Set(providerID, config.ProviderConfig{
			ID: providerID,
			OAuthToken: &oauth.Token{
				AccessToken:  "copilot-inference-token",
				RefreshToken: refreshToken,
			},
			Owner: &config.ProviderOwnerReference{
				Type:         config.ProviderOwnerCore,
				Construction: providerregistry.ConstructionCopilot,
			},
		})
		return config.NewTestStoreWithRegistrations(&config.Config{Providers: providers}, registration).RuntimeSnapshot().Config()
	}
	workspace.cfg = configuration("github-token-before")
	replacement = configuration("github-token-after")
	ui := newTestUI()
	ui.com.Workspace = workspace

	cmd := ui.fetchProviderUsageFor(providerID)
	require.NotNil(t, cmd)
	msg, ok := cmd().(usageUpdatedMsg)
	require.True(t, ok)
	require.Nil(t, msg.usage)
}

func TestProviderUsageRejectsSameIDOwnerReplacementBeforeCallback(t *testing.T) {
	const providerID = "same-id-provider"
	ownerACalls := 0
	ownerBCalls := 0
	registration := func(pluginID string, calls *int) providerregistry.Registration {
		return providerregistry.Registration{
			ProviderID:       providerID,
			AccountNamespace: providerID + "-account",
			Construction:     providerregistry.ConstructionGenericJSON,
			Manifest:         &manifest.Manifest{ID: pluginID, Version: "1.0.0"},
			OAuth:            &providerregistry.OAuthCapability{},
			Quota: func(context.Context, string) (*oauthusage.Usage, error) {
				*calls++
				return &oauthusage.Usage{Windows: []oauthusage.Window{{Name: "quota", Percent: 50}}}, nil
			},
		}
	}
	configuration := func(owner providerregistry.Registration, token string) *config.Config {
		providers := csync.NewMap[string, config.ProviderConfig]()
		providers.Set(providerID, config.ProviderConfig{
			ID:         providerID,
			OAuthToken: &oauth.Token{AccessToken: token},
			Owner: &config.ProviderOwnerReference{
				Type:         config.ProviderOwnerPlugin,
				Construction: owner.Construction,
			},
			Plugin: &config.ProviderPluginReference{ID: owner.Manifest.ID, Version: owner.Manifest.Version},
		})
		return config.NewTestStoreWithRegistrations(&config.Config{Providers: providers}, owner).RuntimeSnapshot().Config()
	}

	ownerA := registration("plugin.owner-a", &ownerACalls)
	ownerB := registration("plugin.owner-b", &ownerBCalls)
	workspace := &testWorkspace{cfg: configuration(ownerA, "owner-a-token")}
	ui := newTestUI()
	ui.com.Workspace = workspace
	cmd := ui.fetchProviderUsageFor(providerID)
	require.NotNil(t, cmd)

	workspace.cfg = configuration(ownerB, "owner-b-token")
	message, ok := cmd().(usageUpdatedMsg)
	require.True(t, ok)
	require.Nil(t, message.usage)
	require.Zero(t, ownerACalls)
	require.Zero(t, ownerBCalls)
}

func TestHandleSelectModelRejectsStaleSameIDOwner(t *testing.T) {
	const providerID = "same-id-provider"
	registration := func(pluginID string) providerregistry.Registration {
		return providerregistry.Registration{
			ProviderID:   providerID,
			Name:         "Synthetic Provider",
			Construction: providerregistry.ConstructionGenericJSON,
			Manifest:     &manifest.Manifest{ID: pluginID, Version: "1.0.0"},
		}
	}
	configuration := func(owner providerregistry.Registration) *config.Config {
		provider := config.ProviderConfig{
			ID:     providerID,
			Name:   owner.Name,
			Models: []catalog.Model{{ID: "shared-model", Name: "Shared Model"}},
			Owner: &config.ProviderOwnerReference{
				Type:         config.ProviderOwnerPlugin,
				Construction: owner.Construction,
			},
			Plugin: &config.ProviderPluginReference{ID: owner.Manifest.ID, Version: owner.Manifest.Version},
		}
		cfg := &config.Config{
			Providers: csync.NewMapFrom(map[string]config.ProviderConfig{providerID: provider}),
			Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {Provider: providerID, Model: "shared-model"},
			},
		}
		return config.NewTestStoreWithRegistrations(cfg, owner).RuntimeSnapshot().Config()
	}

	ownerA := registration("plugin.owner-a")
	ownerB := registration("plugin.owner-b")
	cfgA := configuration(ownerA)
	cfgB := configuration(ownerB)
	provider, ok := cfgA.Providers.Get(providerID)
	require.True(t, ok)
	capturedOwner, ok := cfgA.ProviderOwner(providerID)
	require.True(t, ok)
	workspace := &testWorkspace{cfg: cfgB}
	ui := newTestUI()
	ui.com.Workspace = workspace

	cmd := ui.handleSelectModel(dialog.ActionSelectModel{
		Provider:         provider.ToProvider(),
		Model:            config.SelectedModel{Provider: providerID, Model: "shared-model"},
		ModelType:        config.SelectedModelTypeLarge,
		ProviderOwner:    capturedOwner,
		ProviderOwnerSet: true,
	})
	require.NotNil(t, cmd)
	message, ok := cmd().(util.InfoMsg)
	require.True(t, ok)
	require.Equal(t, util.InfoTypeError, message.Type)
	require.Contains(t, message.Msg, "provider owner changed")
	require.Zero(t, workspace.preferredModelCalls)
}

func modelSelectionTestUI(t *testing.T, includeSmall bool) (*UI, *testWorkspace, dialog.ActionSelectModel, config.AgentModelState) {
	t.Helper()

	const providerID = "copilot"
	registration := providerregistry.Registration{
		ProviderID:   providerID,
		Name:         "GitHub Copilot",
		Construction: providerregistry.ConstructionCopilot,
	}
	provider := config.ProviderConfig{
		ID:   providerID,
		Name: registration.Name,
		Models: []catalog.Model{
			{ID: "large-current", Name: "Large Current"},
			{ID: "large-next", Name: "Large Next"},
			{ID: "small-model", Name: "Small Model"},
		},
		Owner: &config.ProviderOwnerReference{
			Type:         config.ProviderOwnerCore,
			Construction: registration.Construction,
		},
	}
	models := map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeLarge: {Provider: providerID, Model: "large-current"},
	}
	if includeSmall {
		models[config.SelectedModelTypeSmall] = config.SelectedModel{Provider: providerID, Model: "small-model"}
	}
	cfg := &config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{providerID: provider}),
		Models:    models,
	}
	bound := config.NewTestStoreWithRegistrations(cfg, registration).RuntimeSnapshot().Config()
	owner := registration.Owner()
	selection := config.SelectedModel{Provider: providerID, Model: "large-next"}
	state := bound.AgentModelState()
	state.Large = &config.OwnedSelectedModel{Model: selection, Owner: owner}
	workspace := &testWorkspace{
		cfg:                   bound,
		preferredModelResults: []config.AgentModelState{state},
		defaultSmallModel:     config.SelectedModel{Provider: providerID, Model: "small-model"},
	}
	ui := newTestUI()
	ui.com.Workspace = workspace
	ui.dialog = dialog.NewOverlay()
	ui.header = newHeader(ui.com)
	ui.themeKey = styles.ThemeKeyForProvider(providerID)
	return ui, workspace, dialog.ActionSelectModel{
		Provider:         provider.ToProvider(),
		Model:            selection,
		ModelType:        config.SelectedModelTypeLarge,
		ProviderOwner:    owner,
		ProviderOwnerSet: true,
	}, state
}

func collectCommandMessages(command tea.Cmd) []tea.Msg {
	if command == nil {
		return nil
	}
	message := command()
	if message == nil {
		return nil
	}
	if batch, ok := message.(tea.BatchMsg); ok {
		var messages []tea.Msg
		for _, child := range batch {
			messages = append(messages, collectCommandMessages(child)...)
		}
		return messages
	}
	return []tea.Msg{message}
}

func requireCommandError(t *testing.T, messages []tea.Msg, expected string) {
	t.Helper()
	for _, message := range messages {
		if info, ok := message.(util.InfoMsg); ok && info.Type == util.InfoTypeError && strings.Contains(info.Msg, expected) {
			return
		}
	}
	t.Fatalf("expected error containing %q in %#v", expected, messages)
}

func TestHandleSelectModelStopsOnboardingAfterFailedRequiredStage(t *testing.T) {
	for _, test := range []struct {
		name              string
		includeSmall      bool
		configure         func(*testWorkspace)
		expectedWrites    []config.SelectedModelType
		expectedInitCalls int
		expectedError     string
	}{
		{
			name:         "initial model write",
			includeSmall: true,
			configure: func(workspace *testWorkspace) {
				workspace.preferredModelErrors = []error{errors.New("large write failed")}
			},
			expectedWrites: []config.SelectedModelType{config.SelectedModelTypeLarge},
			expectedError:  "large write failed",
		},
		{
			name: "required small model write",
			configure: func(workspace *testWorkspace) {
				workspace.preferredModelErrors = []error{nil, errors.New("small write failed")}
			},
			expectedWrites: []config.SelectedModelType{config.SelectedModelTypeLarge, config.SelectedModelTypeSmall},
			expectedError:  "small write failed",
		},
		{
			name:         "coder initialization",
			includeSmall: true,
			configure: func(workspace *testWorkspace) {
				workspace.initCoderError = errors.New("coder initialization failed")
			},
			expectedWrites:    []config.SelectedModelType{config.SelectedModelTypeLarge},
			expectedInitCalls: 1,
			expectedError:     "coder initialization failed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ui, workspace, action, _ := modelSelectionTestUI(t, test.includeSmall)
			ui.state = uiOnboarding
			test.configure(workspace)

			messages := collectCommandMessages(ui.handleSelectModel(action))

			requireCommandError(t, messages, test.expectedError)
			require.Equal(t, test.expectedWrites, workspace.preferredModelTypes)
			require.Equal(t, test.expectedInitCalls, workspace.initCoderCalls)
			require.Zero(t, workspace.updateAgentCalls)
			require.Equal(t, uiOnboarding, ui.state)
			require.Zero(t, ui.usageFetchGen)
		})
	}
}

func TestHandleSelectModelPublishesUsageOnlyAfterRuntimeUpdate(t *testing.T) {
	t.Run("runtime update failure", func(t *testing.T) {
		ui, workspace, action, expectedState := modelSelectionTestUI(t, true)
		workspace.updateAgentError = errors.New("runtime update failed")

		messages := collectCommandMessages(ui.handleSelectModel(action))

		requireCommandError(t, messages, "runtime update failed")
		require.Equal(t, 1, workspace.updateAgentCalls)
		require.Equal(t, expectedState, workspace.updateAgentState)
		require.Zero(t, ui.usageFetchGen)
		for _, message := range messages {
			_, applied := message.(modelSelectionAppliedMsg)
			require.False(t, applied)
		}
	})

	t.Run("runtime update success", func(t *testing.T) {
		ui, workspace, action, expectedState := modelSelectionTestUI(t, true)
		messages := collectCommandMessages(ui.handleSelectModel(action))
		require.Len(t, messages, 1)
		applied, ok := messages[0].(modelSelectionAppliedMsg)
		require.True(t, ok)
		require.Equal(t, 1, workspace.updateAgentCalls)
		require.Equal(t, expectedState, workspace.updateAgentState)
		require.Zero(t, ui.usageFetchGen)

		_, command := ui.Update(applied)

		require.NotNil(t, command)
		require.Equal(t, uint64(1), ui.usageFetchGen)
		require.Equal(t, "copilot", applied.providerID)
		require.Equal(t, config.SelectedModelTypeLarge, applied.modelType)
		require.Equal(t, "Large Next", applied.modelName)
	})
}

func TestReAuthenticateNotificationRequiresExactOwner(t *testing.T) {
	const providerID = "same-id-provider"
	registration := func(version string) providerregistry.Registration {
		return providerregistry.Registration{
			ProviderID:   providerID,
			Name:         "Synthetic Provider",
			Construction: providerregistry.ConstructionGenericJSON,
			Manifest:     &manifest.Manifest{ID: "plugin.same-id", Version: version},
		}
	}
	configuration := func(active providerregistry.Registration) *config.Config {
		provider := config.ProviderConfig{
			ID:     providerID,
			Name:   active.Name,
			Models: []catalog.Model{{ID: "shared-model", Name: "Shared Model"}},
			Owner: &config.ProviderOwnerReference{
				Type:         config.ProviderOwnerPlugin,
				Construction: active.Construction,
			},
			Plugin: &config.ProviderPluginReference{ID: active.Manifest.ID, Version: active.Manifest.Version},
		}
		cfg := &config.Config{
			Providers: csync.NewMapFrom(map[string]config.ProviderConfig{providerID: provider}),
			Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {Provider: providerID, Model: "shared-model"},
			},
			Agents: map[string]config.Agent{
				config.AgentCoder: {ID: config.AgentCoder, Model: config.SelectedModelTypeLarge},
			},
		}
		return config.NewTestStoreWithRegistrations(cfg, active).RuntimeSnapshot().Config()
	}

	ownerA := registration("1.0.0").Owner()
	ownerB := registration("2.0.0").Owner()
	providerMismatch := ownerB
	providerMismatch.ProviderID = "other-provider"
	for _, test := range []struct {
		name  string
		owner providerregistry.RegistrationOwner
		open  bool
	}{
		{name: "missing owner"},
		{name: "provider mismatch", owner: providerMismatch},
		{name: "same ID stale version", owner: ownerA},
		{name: "exact owner", owner: ownerB, open: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ui := newTestUI()
			ui.com.Workspace = &testWorkspace{cfg: configuration(registration("2.0.0"))}
			ui.dialog = dialog.NewOverlay()

			cmd := ui.handleAgentNotification(notify.Notification{
				Type:       notify.TypeReAuthenticate,
				ProviderID: providerID,
				Owner:      test.owner,
			})
			require.Equal(t, test.open, ui.dialog.ContainsDialog(dialog.APIKeyInputID))
			if !test.open {
				require.Nil(t, cmd)
				require.False(t, ui.dialog.HasDialogs())
			}
		})
	}
}

func TestInitialUIStateRequiresCoderBeforeEnteringWorkspace(t *testing.T) {
	t.Parallel()

	require.Equal(t, uiOnboarding, initialUIState(&testWorkspace{}))
	require.Equal(t, uiInitialize, initialUIState(&testWorkspace{ready: true, needsInitialization: true}))
	require.Equal(t, uiLanding, initialUIState(&testWorkspace{ready: true}))
}

func TestInitialPromptWaitsForStartupGatesAndSubmitsOnce(t *testing.T) {
	t.Parallel()

	for _, state := range []uiState{uiOnboarding, uiInitialize} {
		ui := &UI{state: state, initialPrompt: "prompt"}
		require.Nil(t, ui.sendInitialPrompt())
		require.Equal(t, "prompt", ui.initialPrompt)
	}

	ui := &UI{state: uiLanding, initialPrompt: "prompt"}
	cmd := ui.sendInitialPrompt()
	require.NotNil(t, cmd)
	require.Empty(t, ui.initialPrompt)
	require.Equal(t, sendMessageMsg{Content: "prompt"}, cmd())
	require.Nil(t, ui.sendInitialPrompt(), "initial prompt must be consumed exactly once")
}

func TestContinueStartupLoadsRequestedSessionBeforePrompt(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.state = uiLanding
	ui.initialSessionID = "requested-session"
	ui.initialPrompt = "pending"

	cmd := ui.continueStartup()
	require.NotNil(t, cmd)
	require.Empty(t, ui.initialSessionID)
	require.Equal(t, "pending", ui.initialPrompt)
	require.Nil(t, ui.loadInitialSession(), "startup session must be consumed exactly once")
}

func TestContinueStartupConsumesContinueLastOnce(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.state = uiLanding
	ui.continueLastSession = true
	ui.initialPrompt = "pending"

	cmd := ui.continueStartup()
	require.NotNil(t, cmd)
	require.False(t, ui.continueLastSession)
	require.Equal(t, "pending", ui.initialPrompt)
	require.Nil(t, ui.loadInitialSession(), "continue-last request must be consumed exactly once")
}

func TestContinueStartupWaitsForProjectInitialization(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.state = uiInitialize
	ui.initialSessionID = "requested-session"
	ui.initialPrompt = "pending"

	require.Nil(t, ui.continueStartup())
	require.Equal(t, "requested-session", ui.initialSessionID)
	require.Equal(t, "pending", ui.initialPrompt)
}

func TestProjectInitializationResolutionKeepsStartupSession(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		resolve func(*UI) tea.Cmd
	}{
		{name: "initialize", resolve: (*UI).initializeProject},
		{name: "skip", resolve: (*UI).skipInitializeProject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ui := newTestUI()
			ui.com.Workspace = &testWorkspace{initializePrompt: "initialize"}
			ui.state = uiInitialize
			ui.session = &session.Session{ID: "startup-session"}
			ui.initialSessionID = "requested-session"
			ui.initialPrompt = "pending"

			cmd := tc.resolve(ui)
			require.NotNil(t, cmd)
			require.Equal(t, uiLanding, ui.state)
			require.Equal(t, "startup-session", ui.session.ID, "startup initialization must not replace the pending prompt session")
			require.Empty(t, ui.initialSessionID, "requested session must be queued before startup prompts")
			require.Empty(t, ui.initialPrompt, "pending prompt must be queued exactly once after resolution")
			require.Nil(t, ui.sendInitialPrompt())
		})
	}
}

func TestInitializeButtonsRespondToMouseClicks(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		index    int
		selected bool
	}{
		{name: "initialize", index: 0, selected: true},
		{name: "skip", index: 1, selected: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ui := newTestUI()
			ui.com.Workspace = &testWorkspace{
				cfg:              &config.Config{Options: &config.Options{InitializeAs: "AGENTS.md"}},
				initializePrompt: "initialize",
			}
			ui.state = uiInitialize
			ui.layout.main = uv.Rect(5, 3, 85, 27)
			ui.initializeView()

			x, y := findInitializeButtonPoint(t, ui, test.index)
			cmd, handled := ui.handleInitializeClick(tea.MouseClickMsg(tea.Mouse{X: x, Y: y, Button: uv.MouseLeft}))
			require.True(t, handled)
			require.NotNil(t, cmd)
			require.Equal(t, test.selected, ui.onboarding.yesInitializeSelected)
			require.Equal(t, uiLanding, ui.state)
		})
	}
}

func TestInitializeButtonsIgnoreNonLeftClicks(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.com.Workspace = &testWorkspace{cfg: &config.Config{Options: &config.Options{InitializeAs: "AGENTS.md"}}}
	ui.state = uiInitialize
	ui.layout.main = uv.Rect(5, 3, 85, 27)
	ui.initializeView()
	x, y := findInitializeButtonPoint(t, ui, 0)

	cmd, handled := ui.handleInitializeClick(tea.MouseClickMsg(tea.Mouse{X: x, Y: y, Button: uv.MouseRight}))
	require.False(t, handled)
	require.Nil(t, cmd)
	require.Equal(t, uiInitialize, ui.state)
	require.False(t, ui.onboarding.yesInitializeSelected)
}

func TestInitializeButtonHoverDoesNotChangeSelection(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.com.Workspace = &testWorkspace{cfg: &config.Config{Options: &config.Options{InitializeAs: "AGENTS.md"}}}
	ui.dialog = dialog.NewOverlay()
	ui.state = uiInitialize
	ui.layout.main = uv.Rect(5, 3, 85, 27)
	ui.initializeView()
	x, y := findInitializeButtonPoint(t, ui, 1)

	_, _ = ui.Update(tea.MouseMotionMsg(tea.Mouse{X: x, Y: y}))
	require.Equal(t, 2, ui.onboarding.hoveredInitializeButton)
	require.False(t, ui.onboarding.yesInitializeSelected)
	require.Equal(t, uiInitialize, ui.state)

	_, _ = ui.Update(tea.MouseMotionMsg(tea.Mouse{X: -1, Y: -1}))
	require.Zero(t, ui.onboarding.hoveredInitializeButton)
	require.False(t, ui.onboarding.yesInitializeSelected)
	require.Equal(t, uiInitialize, ui.state)
}

func findInitializeButtonPoint(t *testing.T, ui *UI, index int) (int, int) {
	t.Helper()
	require.NotNil(t, ui.onboarding.buttonCompositor)
	for y := ui.layout.main.Min.Y; y < ui.layout.main.Max.Y; y++ {
		for x := ui.layout.main.Min.X; x < ui.layout.main.Max.X; x++ {
			if common.HitButtonIndex(ui.onboarding.buttonCompositor, x, y) == index {
				return x, y
			}
		}
	}
	t.Fatalf("button %d has no hit target", index)
	return 0, 0
}

func TestCurrentModelSupportsImages(t *testing.T) {
	t.Parallel()

	t.Run("returns false when config is nil", func(t *testing.T) {
		t.Parallel()

		ui := newTestUIWithConfig(t, nil)
		require.False(t, ui.currentModelSupportsImages())
	})

	t.Run("returns false when coder agent is missing", func(t *testing.T) {
		t.Parallel()

		cfg := &config.Config{
			Providers: csync.NewMap[string, config.ProviderConfig](),
			Agents:    map[string]config.Agent{},
		}
		ui := newTestUIWithConfig(t, cfg)
		require.False(t, ui.currentModelSupportsImages())
	})

	t.Run("returns false when model is not found", func(t *testing.T) {
		t.Parallel()

		cfg := &config.Config{
			Providers: csync.NewMap[string, config.ProviderConfig](),
			Agents: map[string]config.Agent{
				config.AgentCoder: {Model: config.SelectedModelTypeLarge},
			},
		}
		ui := newTestUIWithConfig(t, cfg)
		require.False(t, ui.currentModelSupportsImages())
	})

	t.Run("returns true when current model supports images", func(t *testing.T) {
		t.Parallel()

		providers := csync.NewMap[string, config.ProviderConfig]()
		providers.Set("test-provider", config.ProviderConfig{
			ID: "test-provider",
			Models: []catalog.Model{
				{ID: "test-model", SupportsImages: true},
			},
		})

		cfg := &config.Config{
			Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {
					Provider: "test-provider",
					Model:    "test-model",
				},
			},
			Providers: providers,
			Agents: map[string]config.Agent{
				config.AgentCoder: {Model: config.SelectedModelTypeLarge},
			},
		}

		ui := newTestUIWithConfig(t, cfg)
		require.True(t, ui.currentModelSupportsImages())
	})
}

func TestCurrentImageExtensionsFollowSelectedProvider(t *testing.T) {
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("codex", config.ProviderConfig{})
	cfg := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: "codex", Model: "gpt-5"},
		},
		Providers: providers,
		Agents: map[string]config.Agent{
			config.AgentCoder: {Model: config.SelectedModelTypeLarge},
		},
	}
	ui := newTestUIWithConfig(t, cfg)

	require.Equal(t, []string{".gif", ".jpeg", ".jpg", ".png", ".webp"}, ui.currentImageExtensions())
	require.Equal(t, int64(25*1024*1024), ui.currentImageSourceLimit())
	require.True(t, hasAllowedImageExtension("/tmp/IMAGE.WEBP", ui.currentImageExtensions()))
	require.False(t, hasAllowedImageExtension("/tmp/image.webp.exe", ui.currentImageExtensions()))

	for _, test := range []struct {
		name     string
		provider config.ProviderConfig
	}{
		{name: "preset", provider: config.ProviderConfig{Preset: &config.ProviderPresetReference{ID: "example.codex-preset", Version: "1"}}},
		{name: "plugin", provider: config.ProviderConfig{Plugin: &config.ProviderPluginReference{ID: "example.codex-plugin", Version: "1"}}},
		{
			name: "custom",
			provider: config.ProviderConfig{Owner: &config.ProviderOwnerReference{
				Type:         config.ProviderOwnerCustom,
				Construction: providerregistry.ConstructionOpenAICompat,
			}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			providers.Set("codex", test.provider)
			require.Equal(t, []string{".jpeg", ".jpg", ".png"}, ui.currentImageExtensions())
			require.Equal(t, imageattachment.DefaultMaxSourceBytes, ui.currentImageSourceLimit())
			require.False(t, hasAllowedImageExtension("/tmp/image.webp", ui.currentImageExtensions()))
		})
	}
}

func TestCurrentImagePolicyUsesDefaultsForIncompleteSelection(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		cfg  *config.Config
	}{
		{name: "missing config"},
		{name: "missing agent", cfg: &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}},
		{
			name: "missing model",
			cfg: &config.Config{
				Providers: csync.NewMap[string, config.ProviderConfig](),
				Agents: map[string]config.Agent{
					config.AgentCoder: {Model: config.SelectedModelTypeLarge},
				},
			},
		},
		{
			name: "missing provider storage",
			cfg: &config.Config{
				Models: map[config.SelectedModelType]config.SelectedModel{
					config.SelectedModelTypeLarge: {Provider: "codex", Model: "gpt-5"},
				},
				Agents: map[string]config.Agent{
					config.AgentCoder: {Model: config.SelectedModelTypeLarge},
				},
			},
		},
		{
			name: "missing provider",
			cfg: &config.Config{
				Models: map[config.SelectedModelType]config.SelectedModel{
					config.SelectedModelTypeLarge: {Provider: "codex", Model: "gpt-5"},
				},
				Providers: csync.NewMap[string, config.ProviderConfig](),
				Agents: map[string]config.Agent{
					config.AgentCoder: {Model: config.SelectedModelTypeLarge},
				},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ui := &UI{com: &common.Common{Workspace: &testWorkspace{}}}
			if test.cfg != nil {
				ui = newTestUIWithConfig(t, test.cfg)
			}
			require.Equal(t, []string{".jpeg", ".jpg", ".png"}, ui.currentImageExtensions())
			require.Equal(t, imageattachment.DefaultMaxSourceBytes, ui.currentImageSourceLimit())
		})
	}
}

func TestChatHighlightCopiedClearsMouseStateInUpdate(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.chat.mouseDown = true
	ui.chat.mouseDownItem = 2
	ui.chat.mouseDragItem = 3
	ui.chat.clickCount = 2

	_, _ = ui.Update(chatHighlightCopiedMsg{})

	require.False(t, ui.chat.mouseDown)
	require.Equal(t, -1, ui.chat.mouseDownItem)
	require.Equal(t, -1, ui.chat.mouseDragItem)
	require.Zero(t, ui.chat.clickCount)
}

func newTestUIWithConfig(t *testing.T, cfg *config.Config) *UI {
	t.Helper()

	bound := config.NewTestStoreWithRegistrations(cfg, providerregistry.Integrated()...).RuntimeSnapshot().Config()
	return &UI{
		com: &common.Common{
			Workspace: &testWorkspace{cfg: bound},
		},
	}
}

// testWorkspace is a minimal [workspace.Workspace] stub for unit tests.
type testWorkspace struct {
	workspace.Workspace
	cfg                   *config.Config
	ready                 bool
	needsInitialization   bool
	initializePrompt      string
	markedInitialized     int
	preferredModelCalls   int
	preferredModelTypes   []config.SelectedModelType
	preferredModelResults []config.AgentModelState
	preferredModelErrors  []error
	defaultSmallModel     config.SelectedModel
	defaultSmallError     error
	defaultSmallCalls     int
	updateAgentCalls      int
	updateAgentState      config.AgentModelState
	updateAgentError      error
	initCoderCalls        int
	initCoderError        error
}

func (w *testWorkspace) WorkingDir() string {
	return "/workspace"
}

func (w *testWorkspace) Config() *config.Config {
	return w.cfg
}

func (w *testWorkspace) ProviderSurfaces() []providerregistry.Surface {
	return nil
}

func (w *testWorkspace) UpdatePreferredModel(_ config.Scope, modelType config.SelectedModelType, model config.SelectedModel, owner providerregistry.RegistrationOwner) (config.AgentModelState, error) {
	call := w.preferredModelCalls
	w.preferredModelCalls++
	w.preferredModelTypes = append(w.preferredModelTypes, modelType)
	if call < len(w.preferredModelErrors) && w.preferredModelErrors[call] != nil {
		return config.AgentModelState{}, w.preferredModelErrors[call]
	}
	if call < len(w.preferredModelResults) {
		return w.preferredModelResults[call], nil
	}
	owned := &config.OwnedSelectedModel{Model: model, Owner: owner}
	state := config.AgentModelState{}
	if modelType == config.SelectedModelTypeSmall {
		state.Small = owned
	} else {
		state.Large = owned
	}
	return state, nil
}

func (w *testWorkspace) GetDefaultSmallModel(string) (config.SelectedModel, error) {
	w.defaultSmallCalls++
	return w.defaultSmallModel, w.defaultSmallError
}

func (w *testWorkspace) UpdateAgentModel(_ context.Context, state config.AgentModelState) error {
	w.updateAgentCalls++
	w.updateAgentState = state
	return w.updateAgentError
}

func (w *testWorkspace) InitCoderAgent(context.Context) error {
	w.initCoderCalls++
	return w.initCoderError
}

func (w *testWorkspace) AgentIsReady() bool {
	return w.ready
}

func (w *testWorkspace) ProjectNeedsInitialization() (bool, error) {
	return w.needsInitialization, nil
}

func (w *testWorkspace) MarkProjectInitialized() error {
	w.markedInitialized++
	return nil
}

func (w *testWorkspace) InitializePrompt() (string, error) {
	return w.initializePrompt, nil
}
