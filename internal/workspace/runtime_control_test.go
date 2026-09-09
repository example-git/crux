package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func runtimeControlWorkspaceFixture(t *testing.T, value string) (*config.Config, providerregistry.Registration, config.RuntimeControlState) {
	t.Helper()
	control := manifest.RuntimeControl{ID: "vendor.control", Type: "string", Scope: "model", RequestPath: "/control"}
	registration := providerregistry.Registration{ProviderID: "fixture", Construction: providerregistry.ConstructionOpenAIResponses, Manifest: &manifest.Manifest{ID: "fixture.plugin", Version: "1.0.0"}, RuntimeControls: []manifest.RuntimeControl{control}, Runtime: &providerregistry.RuntimeCapability{Available: func(string) bool { return true }}}
	cfg := &config.Config{Options: &config.Options{}, Providers: csync.NewMapFrom(map[string]config.ProviderConfig{"fixture": {ID: "fixture", Type: catalog.TypeOpenAI, Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: providerregistry.ConstructionOpenAIResponses}, Plugin: &config.ProviderPluginReference{ID: "fixture.plugin", Version: "1.0.0"}, APIKey: "synthetic-key", Models: []catalog.Model{{ID: "model", Name: "Model", ContextWindow: 8192, DefaultMaxTokens: 1024}}}}), Models: map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeLarge: {Provider: "fixture", Model: "model", ProviderOptions: map[string]any{control.ID: value}},
		config.SelectedModelTypeSmall: {Provider: "fixture", Model: "model"},
	}}
	store := config.NewTestStoreWithRegistrations(cfg, registration)
	target := config.RuntimeControlTarget{Owner: registration.Owner(), ControlID: control.ID, Selection: config.RuntimeControlSelection{ModelType: config.SelectedModelTypeLarge, ModelID: "model"}}
	state, err := store.Config().RuntimeControlEffectiveState(config.ScopeGlobal, target)
	require.NoError(t, err)
	return store.Config(), registration, state
}

func TestRuntimeControlWorkspaceRequiresMatchingRefreshedState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	for _, mode := range []string{"accepted", "refresh rejected", "owner changed", "model changed", "effective changed", "descriptor changed", "workspace changed"} {
		t.Run(mode, func(t *testing.T) {
			cfg, registration, state := runtimeControlWorkspaceFixture(t, "accepted")
			state.ScopedKnown = true
			state.Scoped = state.Effective
			binding := state.Binding
			surface := providerregistry.Surface{ID: registration.ProviderID, Owner: &state.Target.Owner, Available: true, RuntimeControls: []providerregistry.RuntimeControlSurface{{RuntimeControl: registration.RuntimeControls[0], Available: true, AvailableModels: []string{"model"}, Binding: &binding, DescriptorDigest: state.Target.DescriptorDigest}}}
			current := proto.Workspace{ID: "fixture", Config: cfg, ProviderSurfaces: []providerregistry.Surface{surface}}
			switch mode {
			case "owner changed":
				owner := state.Target.Owner
				owner.ManifestVersion = "replacement"
				current.ProviderSurfaces[0].Owner = &owner
			case "model changed":
				model := cfg.Models[config.SelectedModelTypeLarge]
				model.Model = "other"
				cfg.Models[config.SelectedModelTypeLarge] = model
			case "effective changed":
				cfg.Models[config.SelectedModelTypeLarge].ProviderOptions["vendor.control"] = "other"
			case "descriptor changed":
				current.ProviderSurfaces[0].RuntimeControls[0].DescriptorDigest = "other"
			case "workspace changed":
				current.ID = "replacement"
			}
			reads, writes := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					reads++
					if mode == "refresh rejected" {
						http.Error(w, "synthetic refresh failure", http.StatusServiceUnavailable)
						return
					}
					require.NoError(t, json.NewEncoder(w).Encode(current))
					return
				}
				writes++
				require.Equal(t, http.MethodPut, r.Method)
				require.NoError(t, json.NewEncoder(w).Encode(state))
			}))
			defer srv.Close()
			address, err := url.Parse(srv.URL)
			require.NoError(t, err)
			sdk, err := client.NewClient(t.TempDir(), "tcp", address.Host)
			require.NoError(t, err)
			w := NewClientWorkspace(sdk, proto.Workspace{ID: "fixture"})
			defer w.subCancel()
			_, err = w.SetRuntimeControl(t.Context(), state.Scope, state.Target, state.Effective.Value)
			if mode == "accepted" {
				require.NoError(t, err)
				require.NoError(t, validateRuntimeControlPublicView(state, w.Config(), w.ProviderSurfaces()))
			} else {
				require.Error(t, err)
				require.Nil(t, w.Config())
			}
			require.Equal(t, 1, reads)
			require.Equal(t, 1, writes)
		})
	}
}

func TestRuntimeControlOwningResolveRejectsUnacknowledgedLocalState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	for _, mode := range []string{"accepted", "local changed", "private control changed", "private owner changed", "private model changed"} {
		t.Run(mode, func(t *testing.T) {
			accepted, reg, state := runtimeControlWorkspaceFixture(t, "accepted")
			local, _, _ := runtimeControlWorkspaceFixture(t, "accepted")
			if mode == "local changed" {
				local.Models[config.SelectedModelTypeLarge].ProviderOptions["vendor.control"] = "saved-but-rejected"
			}
			provider, _ := accepted.Providers.Get(reg.ProviderID)
			proposal := config.RemoteRuntimeProposal{Models: accepted.Models, Providers: []config.RemoteProviderDefinition{{Config: provider}}, Credentials: []config.RemoteCredentialBinding{{Owner: reg.Owner()}}}
			switch mode {
			case "private control changed":
				proposal.Controls.AnalysisEffort = "high"
			case "private owner changed":
				proposal.Credentials[0].Owner.ManifestVersion = "replacement"
			case "private model changed":
				proposal.Models = map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: reg.ProviderID, Model: "other"}}
			}
			w := NewClientWorkspace(nil, proto.Workspace{ID: "fixture", Authority: &config.RemoteAuthority{Mode: "client"}})
			defer w.subCancel()
			w.authority = &clientAuthority{store: config.NewTestStoreWithRegistrations(local, reg), accepted: proposal}
			w.authority.view.Store(accepted)
			// No pending marker and no SDK: resolve must compare accepted/local
			// state and may never silently publish a prior rejected disk edit.
			result, err := w.RuntimeControlState(t.Context(), state.Scope, state.Target)
			if mode == "accepted" {
				require.NoError(t, err)
				require.False(t, result.ScopedKnown)
				require.False(t, result.Scoped.Present)
			} else {
				require.Error(t, err)
			}
			require.Nil(t, w.authority.pending)
		})
	}
}

func TestRuntimeControlWorkspaceRejectsShutdownAndMissingAuthority(t *testing.T) {
	w := NewClientWorkspace(nil, proto.Workspace{ID: "fixture"})
	w.subCancel()
	_, err := w.RuntimeControlState(t.Context(), config.ScopeGlobal, config.RuntimeControlTarget{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = w.SetRuntimeControl(t.Context(), config.ScopeGlobal, config.RuntimeControlTarget{}, json.RawMessage(`false`))
	require.ErrorIs(t, err, context.Canceled)
	_, err = w.RemoveRuntimeControl(t.Context(), config.ScopeGlobal, config.RuntimeControlTarget{})
	require.ErrorIs(t, err, context.Canceled)
	w = NewClientWorkspace(nil, proto.Workspace{ID: "fixture", Authority: &config.RemoteAuthority{Mode: "client"}})
	defer w.subCancel()
	target := config.RuntimeControlTarget{Owner: providerregistry.RegistrationOwner{ProviderID: "fixture"}, ControlID: "control", DescriptorDigest: "digest", Selection: config.RuntimeControlSelection{ModelType: config.SelectedModelTypeLarge, ModelID: "model"}}
	_, err = w.RuntimeControlState(t.Context(), config.ScopeGlobal, target)
	require.ErrorContains(t, err, "owning client")
}
