package workspace

import (
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
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func contextReloadWorkspace(owner providerregistry.RegistrationOwner) proto.Workspace {
	return proto.Workspace{
		ID: "fixture",
		Config: &config.Config{Options: &config.Options{}, Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			owner.ProviderID: {ID: owner.ProviderID},
		})},
		ProviderSurfaces: []providerregistry.Surface{{ID: owner.ProviderID, Owner: &owner, Available: true}},
	}
}

func TestProviderContextReloadRequiresMatchingWorkspaceAndActiveOwner(t *testing.T) {
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture", Construction: providerregistry.ConstructionOpenAICompat}
	for _, mode := range []string{"accepted", "mismatched id", "missing config", "different owner", "disabled", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			current := contextReloadWorkspace(owner)
			switch mode {
			case "mismatched id":
				current.ID = "other"
			case "missing config":
				current.Config = nil
			case "different owner":
				other := owner
				other.ManifestVersion = "replacement"
				current.ProviderSurfaces[0].Owner = &other
			case "disabled":
				current.Config.Providers.Set(owner.ProviderID, config.ProviderConfig{ID: owner.ProviderID, Disable: true})
			case "unavailable":
				current.ProviderSurfaces[0].Available = false
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "/v1/workspaces/fixture", r.URL.Path)
				require.NoError(t, json.NewEncoder(w).Encode(current))
			}))
			defer srv.Close()
			address, err := url.Parse(srv.URL)
			require.NoError(t, err)
			sdk, err := client.NewClient(t.TempDir(), "tcp", address.Host)
			require.NoError(t, err)
			w := NewClientWorkspace(sdk, proto.Workspace{ID: "fixture"})
			defer w.subCancel()
			err = w.ReloadProviderContextInstructions(t.Context(), owner)
			if mode == "accepted" {
				require.NoError(t, err)
				require.NoError(t, validateProviderContextOwner(w.Config(), w.ProviderSurfaces(), owner))
			} else {
				require.Error(t, err)
				require.Nil(t, w.Config(), "unacknowledged configuration must not be adopted")
			}
		})
	}
}

func TestProviderContextReloadRejectsConcurrentWorkspaceReplacement(t *testing.T) {
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture", Construction: providerregistry.ConstructionOpenAICompat}
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/workspaces/fixture", r.URL.Path)
		close(started)
		<-release
		require.NoError(t, json.NewEncoder(w).Encode(contextReloadWorkspace(owner)))
	}))
	defer srv.Close()
	address, err := url.Parse(srv.URL)
	require.NoError(t, err)
	sdk, err := client.NewClient(t.TempDir(), "tcp", address.Host)
	require.NoError(t, err)
	w := NewClientWorkspace(sdk, proto.Workspace{ID: "fixture"})
	defer w.subCancel()
	result := make(chan error, 1)
	go func() { result <- w.ReloadProviderContextInstructions(t.Context(), owner) }()
	<-started
	w.mu.Lock()
	w.ws.ID = "replacement"
	w.mu.Unlock()
	close(release)
	require.ErrorContains(t, <-result, "workspace changed")
	require.Equal(t, "replacement", w.workspaceID())
	require.Nil(t, w.Config())
}

func TestProviderContextReloadRejectsUnselectedOwningClientProviderBeforeCollection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	owner := providerregistry.RegistrationOwner{ProviderID: "unselected", Construction: providerregistry.ConstructionOpenAICompat}
	providers := csync.NewMap[string, config.ProviderConfig]()
	for _, id := range []string{"selected", "unselected"} {
		providers.Set(id, config.ProviderConfig{ID: id, Type: catalog.TypeOpenAICompat,
			Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
			Models: []catalog.Model{{ID: "model", Name: "Model"}},
		})
	}
	store := config.NewTestStore(&config.Config{Options: &config.Options{}, Providers: providers, Models: map[config.SelectedModelType]config.SelectedModel{
		config.SelectedModelTypeLarge: {Provider: "selected", Model: "model"},
		config.SelectedModelTypeSmall: {Provider: "selected", Model: "model"},
	}})
	owner, ok := store.Config().ProviderOwner(owner.ProviderID)
	require.True(t, ok)
	require.NoError(t, store.ValidateActiveProviderOwner(owner), "fixture must have an active but unselected provider")
	w := NewClientWorkspace(nil, proto.Workspace{ID: "fixture", Authority: &config.RemoteAuthority{Mode: "client"}})
	defer w.subCancel()
	w.authority = &clientAuthority{store: store}
	w.authority.view.Store(store.Config())
	// The nil SDK makes accidental publication observable; the selection check
	// must happen before collecting files or attempting remote acknowledgement.
	require.ErrorContains(t, w.ReloadProviderContextInstructions(t.Context(), owner), "selected model provider")
	require.Nil(t, w.authority.pending)
}

func TestProviderContextReloadChecksAcceptedBinding(t *testing.T) {
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture", Construction: providerregistry.ConstructionOpenAICompat}
	for _, mode := range []string{"accepted", "explicit empty", "small only", "unselected", "missing context", "missing owner", "different owner"} {
		t.Run(mode, func(t *testing.T) {
			accepted := config.RemoteRuntimeProposal{
				Models:                      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: owner.ProviderID, Model: "model"}},
				ProviderContextInstructions: map[string]string{owner.ProviderID: "Captured instructions"},
				Credentials:                 []config.RemoteCredentialBinding{{Owner: owner}},
			}
			switch mode {
			case "explicit empty":
				accepted.ProviderContextInstructions[owner.ProviderID] = ""
			case "small only":
				accepted.Models = map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeSmall: {Provider: owner.ProviderID, Model: "model"}}
			case "unselected":
				accepted.Models = nil
			case "missing context":
				accepted.ProviderContextInstructions = nil
			case "missing owner":
				accepted.Credentials = nil
			case "different owner":
				accepted.Credentials[0].Owner.ManifestVersion = "replacement"
			}
			err := validateAcceptedProviderContext(accepted, owner)
			if mode == "accepted" || mode == "explicit empty" || mode == "small only" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
