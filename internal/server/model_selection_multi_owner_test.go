package server

import (
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// synthOwnedProviderProposal builds a minimal, self-consistent client
// runtime proposal for a single synthetic openai-compat provider, mirroring
// internal/backend's synthClientProviderProposal test fixture so both
// packages exercise the multi-owner admission path against an equivalent
// shape without exporting a shared test-only helper across packages.
func synthOwnedProviderProposal(t *testing.T, providerID string) config.RemoteRuntimeProposal {
	t.Helper()
	owner := providerregistry.RegistrationOwner{ProviderID: providerID}
	proposal := config.RemoteRuntimeProposal{
		Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers: []config.RemoteProviderDefinition{{Config: config.ProviderConfig{
			ID: providerID, Name: providerID, Type: catalog.TypeOpenAICompat, BaseURL: "http://127.0.0.1:1/v1",
			Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
			Models: []catalog.Model{{ID: "model", Name: "Model", ContextWindow: 8192, DefaultMaxTokens: 128}},
		}}},
		Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: providerID, Model: "model"}, config.SelectedModelTypeSmall: {Provider: providerID, Model: "model"}},
		Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-" + providerID}},
		// The primary owner must explicitly opt in before any distinct
		// principal may attach to this workspace as a secondary owner (see
		// backend.checkWorkspaceReuse); harmless on a secondary's own
		// proposal, which is never itself consulted for this gate.
		AllowSecondaryOwners: true,
	}
	digest, err := config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	proposal.Digest = digest
	return proposal
}

// TestSetModelSelectionEnforcesAsymmetricOwnershipLock exercises the
// server-side usage lock (internal/server/peer_channel.go's
// setModelSelection) directly against a real two-owner client-authority
// workspace, bypassing the websocket handshake since only the rejection and
// acceptance status of the call are under test here, not wire framing.
func TestSetModelSelectionEnforcesAsymmetricOwnershipLock(t *testing.T) {
	server := NewServer(nil, "unix", "test")
	t.Cleanup(server.backend.Shutdown)

	primaryPrincipal := strings.Repeat("a", 64)
	secondaryPrincipal := strings.Repeat("b", 64)
	primaryProposal := synthOwnedProviderProposal(t, "fixture-primary")
	secondaryProposal := synthOwnedProviderProposal(t, "fixture-secondary")

	path := t.TempDir()
	first, _, err := server.backend.CreateWorkspace(proto.Workspace{
		Path: path, DataDir: t.TempDir(), ClientID: uuid.NewString(),
		AuthorityMode: "client", AuthenticatedPrincipal: primaryPrincipal, Runtime: &primaryProposal,
	})
	require.NoError(t, err)
	t.Cleanup(first.Shutdown)

	second, _, err := server.backend.CreateWorkspace(proto.Workspace{
		Path: path, DataDir: t.TempDir(), ClientID: uuid.NewString(),
		AuthorityMode: "client", AuthenticatedPrincipal: secondaryPrincipal, Runtime: &secondaryProposal,
	})
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID, "the secondary owner reuses the existing workspace")

	newPeer := func(principal string) *serverPeerChannel {
		return &serverPeerChannel{
			controller: &controllerV1{}, principal: principal,
			ctx:       t.Context(),
			critical:  make(chan peerChannelOutbound, 16),
			state:     make(chan peerChannelOutbound, 16),
			workspace: make(chan peerChannelOutbound, 16),
			telemetry: make(chan peerChannelOutbound, 16),
			attachments: map[string]*serverPeerAttachment{
				first.ID: {workspace: first, authentication: map[providerauth.Owner]providerauth.Generation{}},
			},
		}
	}

	modelRef := func(t *testing.T, snapshot config.RuntimeSnapshot, providerID string) proto.ModelRef {
		t.Helper()
		definition, owner, err := snapshot.RetainedClientProviderDefinition(providerID)
		require.NoError(t, err)
		digest, err := definition.Digest()
		require.NoError(t, err)
		return proto.ModelRef{
			Provider: proto.ProviderRef{Owner: providerauth.PublicOwner(owner), DefinitionDigest: digest, BundleDigest: definition.BundleDigest},
			ModelID:  "model",
		}
	}

	sendSelection := func(t *testing.T, peer *serverPeerChannel, providerID string) proto.PeerAcknowledgement {
		t.Helper()
		snapshot := first.Cfg.RuntimeSnapshot()
		authority := snapshot.RemoteAuthority()
		ref := modelRef(t, snapshot, providerID)
		settings := config.SelectedModel{Provider: providerID, Model: "model"}
		request := proto.PeerModelSelectionSet{
			Runtime: proto.PeerRuntimeBase{ExpectedRevision: authority.Revision, ExpectedDigest: authority.Digest, ResultRevision: authority.Revision + 1},
			Selections: []proto.PeerModelSelectionOperation{
				{ModelType: config.SelectedModelTypeLarge, Model: ref, Settings: settings},
			},
		}
		request.Runtime.ResultDigest = expectedResultDigestForModelSelection(t, first.Cfg, peer.principal, authority.Revision+1, map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: settings})
		return peer.setModelSelection(first.ID, request)
	}

	// The secondary owner selecting its own contributed provider succeeds.
	ack := sendSelection(t, newPeer(secondaryPrincipal), "fixture-secondary")
	require.Equal(t, proto.WorkspaceChannelStatusOK, ack.Status)
	require.Equal(t, secondaryPrincipal, first.Cfg.RuntimeSnapshot().ProviderOwnerPrincipal("fixture-secondary"))
	require.Equal(t, "fixture-secondary", first.Cfg.RuntimeSnapshot().Config().Models[config.SelectedModelTypeLarge].Provider)

	// The primary owner cannot select the secondary's provider.
	ack = sendSelection(t, newPeer(primaryPrincipal), "fixture-secondary")
	require.Equal(t, proto.WorkspaceChannelStatusForbidden, ack.Status)
	require.Contains(t, ack.Message, "owned by a different attached client")

	// The primary owner can still select its own provider.
	ack = sendSelection(t, newPeer(primaryPrincipal), "fixture-primary")
	require.Equal(t, proto.WorkspaceChannelStatusOK, ack.Status)
	require.Equal(t, "fixture-primary", first.Cfg.RuntimeSnapshot().Config().Models[config.SelectedModelTypeLarge].Provider)

	// The secondary owner cannot select the primary's provider.
	ack = sendSelection(t, newPeer(secondaryPrincipal), "fixture-primary")
	require.Equal(t, proto.WorkspaceChannelStatusForbidden, ack.Status)
	require.Contains(t, ack.Message, "owned by a different attached client")
}

// expectedResultDigestForModelSelection replicates the exact digest
// computation PatchOwnedModelSelections/PatchRemoteModelSelections perform
// server-side for a model-selection patch, so this test can supply a valid
// ResultDigest without depending on unexported config-package internals
// beyond the already-public RuntimeSnapshot/Config surface.
func expectedResultDigestForModelSelection(t *testing.T, store *config.ConfigStore, principal string, resultRevision uint64, selections map[config.SelectedModelType]config.SelectedModel) string {
	t.Helper()
	digest, err := store.PreviewModelSelectionResultDigest(principal, resultRevision, selections)
	if err != nil {
		// The caller is intentionally requesting a provider it does not own
		// in this scenario. setModelSelection's ownership-lock check (see
		// peer_channel.go) rejects such a request before ever reaching the
		// digest-checked patch, so no valid digest can or needs to exist;
		// any placeholder value is fine here.
		return "unused-digest-forbidden-scenario"
	}
	return digest
}
