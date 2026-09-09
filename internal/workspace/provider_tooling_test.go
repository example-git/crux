package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestClientWorkspaceProviderToolingRequiresMatchingRefresh(t *testing.T) {
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture", Construction: providerregistry.ConstructionOpenAICompat}
	for _, mode := range []string{"accepted", "remove inherited", "refresh failure", "profile changed", "owner changed", "disabled", "missing config", "workspace changed"} {
		t.Run(mode, func(t *testing.T) {
			ack := proto.ProviderToolingState{Scope: config.ScopeGlobal, Owner: owner, Profile: config.ToolingInstructionsCrux}
			refreshedOwner := owner
			provider := config.ProviderConfig{ID: owner.ProviderID, ToolingInstructions: config.ToolingInstructionsCrux}
			switch mode {
			case "profile changed":
				provider.ToolingInstructions = config.ToolingInstructionsNative
			case "owner changed":
				refreshedOwner.ManifestVersion = "replacement"
			case "disabled":
				provider.Disable = true
			}
			current := proto.Workspace{ID: "fixture", Config: &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{owner.ProviderID: provider}), Options: &config.Options{}}, ProviderSurfaces: []providerregistry.Surface{{ID: owner.ProviderID, Owner: &refreshedOwner}}}
			if mode == "missing config" {
				current.Config = nil
			}
			if mode == "workspace changed" {
				current.ID = "replacement"
			}
			var writes, reads int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					reads++
					if mode == "refresh failure" {
						http.Error(w, "synthetic refresh failure", http.StatusServiceUnavailable)
						return
					}
					require.NoError(t, json.NewEncoder(w).Encode(current))
					return
				}
				writes++
				method := http.MethodPut
				if mode == "remove inherited" {
					method = http.MethodDelete
				}
				require.Equal(t, method, r.Method)
				require.NoError(t, json.NewEncoder(w).Encode(ack))
			}))
			defer srv.Close()
			u, err := url.Parse(srv.URL)
			require.NoError(t, err)
			c, err := client.NewClient(t.TempDir(), "tcp", u.Host)
			require.NoError(t, err)
			w := NewClientWorkspace(c, proto.Workspace{ID: "fixture"})
			defer w.subCancel()
			if mode == "remove inherited" {
				err = w.RemoveProviderToolingInstructions(config.ScopeGlobal, owner)
			} else {
				err = w.SetProviderToolingInstructions(config.ScopeGlobal, owner, config.ToolingInstructionsCrux)
			}
			if mode == "accepted" || mode == "remove inherited" {
				require.NoError(t, err)
				require.NoError(t, ack.ValidateConfig(w.Config()))
			} else {
				require.Error(t, err)
				require.Nil(t, w.Config(), "unacknowledged refreshed state must not become the local view")
			}
			require.Equal(t, 1, writes)
			require.Equal(t, 1, reads)
		})
	}
}

func TestClientWorkspaceProviderToolingRejectsShutdownAndMissingAuthority(t *testing.T) {
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture"}
	w := NewClientWorkspace(nil, proto.Workspace{ID: "fixture"})
	w.subCancel()
	require.ErrorIs(t, w.SetProviderToolingInstructions(config.ScopeGlobal, owner, config.ToolingInstructionsCrux), context.Canceled)
	require.ErrorIs(t, w.RemoveProviderToolingInstructions(config.ScopeGlobal, owner), context.Canceled)
	w = NewClientWorkspace(nil, proto.Workspace{ID: "fixture", Authority: &config.RemoteAuthority{Mode: "client"}})
	defer w.subCancel()
	require.ErrorContains(t, w.SetProviderToolingInstructions(config.ScopeGlobal, owner, config.ToolingInstructionsCrux), "owning client")
	require.ErrorContains(t, w.RemoveProviderToolingInstructions(config.ScopeGlobal, owner), "owning client")
}

func TestProviderToolingChecksAcceptedProposalContent(t *testing.T) {
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture", Construction: providerregistry.ConstructionOpenAICompat}
	state := proto.ProviderToolingState{Scope: config.ScopeGlobal, Owner: owner, Profile: config.ToolingInstructionsCrux}
	for _, mode := range []string{"accepted", "unselected", "different profile", "different owner", "missing owner", "missing definition", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			proposal := config.RemoteRuntimeProposal{
				Providers:   []config.RemoteProviderDefinition{{Config: config.ProviderConfig{ID: owner.ProviderID, ToolingInstructions: state.Profile}}},
				Credentials: []config.RemoteCredentialBinding{{Owner: owner}},
				Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: owner.ProviderID, Model: "model"}},
			}
			switch mode {
			case "unselected":
				proposal = config.RemoteRuntimeProposal{}
			case "different profile":
				proposal.Providers[0].Config.ToolingInstructions = config.ToolingInstructionsNative
			case "different owner":
				proposal.Credentials[0].Owner.ManifestVersion = "replacement"
			case "missing owner":
				proposal.Credentials = nil
			case "missing definition":
				proposal.Providers = nil
			case "disabled":
				proposal.Providers[0].Config.Disable = true
			}
			err := validateAcceptedProviderTooling(proposal, state)
			if mode == "accepted" || mode == "unselected" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
