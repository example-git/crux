package workspace

import (
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestClientWorkspaceProtocolEventsPreserveWireState(t *testing.T) {
	selected := NewClientWorkspace(nil, proto.Workspace{ID: "remote", Config: &config.Config{}})
	defer selected.subCancel()
	public := pubsub.Event[proto.Session]{Payload: proto.Session{ID: "session", IsBusy: true, AttachedClients: 2}}
	events := make(chan any, 5)
	events <- proto.PeerProviderAuthentication{}
	events <- proto.PeerModelSelectionChanged{}
	events <- proto.PeerRuntimePatchApplied{}
	events <- pubsub.Event[config.ClientRefreshRequest]{}
	events <- public
	close(events)
	var received []any
	selected.consumeProtocolEvents(events, nil, func(event any) {
		received = append(received, event)
	})
	require.Equal(t, []any{public}, received)
}

func TestClientWorkspaceFrontendSnapshotExcludesPrivateRuntime(t *testing.T) {
	authority := &config.RemoteAuthority{Mode: "client", Revision: 7, Digest: "accepted"}
	selected := NewClientWorkspace(nil, proto.Workspace{
		ID:        "remote",
		Path:      "/srv/project",
		Config:    &config.Config{},
		Authority: authority,
		Env:       []string{"PRIVATE_VALUE=private"},
		Runtime:   &config.RemoteRuntimeProposal{},
		Creation:  &proto.Workspace{Env: []string{"PRIVATE_VALUE=private"}},
		ForwardedProviders: map[string]config.ProviderConfig{
			"provider": {APIKey: "private"},
		},
		ForwardedAccounts: map[string]config.ForwardedAccount{"provider": {}},
	})
	defer selected.subCancel()
	view := selected.Snapshot()
	require.Equal(t, "remote", view.ID)
	require.Equal(t, "/srv/project", view.Path)
	require.Equal(t, authority, view.Authority)
	require.NotSame(t, authority, view.Authority)
	require.Nil(t, view.Env)
	require.Nil(t, view.Runtime)
	require.Nil(t, view.Creation)
	require.Nil(t, view.ForwardedProviders)
	require.Nil(t, view.ForwardedAccounts)
	view.Authority.Digest = "changed"
	require.Equal(t, "accepted", selected.cached().Authority.Digest)
	require.NotNil(t, selected.cached().Runtime)
}
