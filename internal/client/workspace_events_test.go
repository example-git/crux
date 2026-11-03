package client

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/example-git/crux/internal/config"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestPublicWorkspaceEventsRoundTrip(t *testing.T) {
	events := []any{
		pubsub.Event[proto.LSPEvent]{},
		pubsub.Event[proto.MCPEvent]{},
		pubsub.Event[proto.PermissionRequest]{},
		pubsub.Event[proto.PermissionNotification]{},
		pubsub.Event[proto.QuestionRequest]{},
		pubsub.Event[proto.QuestionNotification]{},
		pubsub.Event[proto.Message]{Payload: proto.Message{ID: "message", SessionID: "session"}},
		pubsub.Event[proto.Session]{Payload: proto.Session{ID: "session", IsBusy: true, AttachedClients: 2}},
		pubsub.Event[proto.File]{},
		pubsub.Event[proto.AgentEvent]{Payload: proto.AgentEvent{
			Type: proto.AgentEventTypeUsage, ProviderID: "native",
			Owner: &providerregistry.RegistrationOwner{ProviderID: "native"},
			Usage: &config.ProviderUsageResult{Revision: 7, Digest: "runtime", Usage: &oauthusage.Usage{ProviderID: "native", Plan: "current"}},
		}},
		pubsub.Event[proto.ConfigChanged]{},
		pubsub.Event[proto.SkillsEvent]{},
		pubsub.Event[proto.TaskNotification]{},
		pubsub.Event[proto.RunComplete]{Payload: proto.RunComplete{SessionID: "session", RunID: "run", Cancelled: true}},
	}
	for _, event := range events {
		t.Run(fmt.Sprintf("%T", event), func(t *testing.T) {
			payload, err := EncodeWorkspaceEvent(event)
			require.NoError(t, err)
			body, err := json.Marshal(payload)
			require.NoError(t, err)
			decoded, err := decodeWorkspaceEvent(body)
			require.NoError(t, err)
			require.Equal(t, event, decoded)
		})
	}
}

func TestPublicWorkspaceEventsExcludePrivateAuthorityControls(t *testing.T) {
	for _, event := range []any{
		pubsub.Event[config.ClientRefreshRequest]{},
		proto.PeerProviderAuthentication{},
		proto.PeerModelSelectionChanged{},
		proto.PeerRuntimePatchApplied{},
	} {
		payload, err := EncodeWorkspaceEvent(event)
		require.Error(t, err)
		require.Empty(t, payload.Payload)
	}
}
