package proto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

const peerTestEpoch = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestPeerMessageRegistryComplete(t *testing.T) {
	expected := []PeerMessageType{
		PeerTypeHello, PeerTypeReady, PeerTypeHeartbeat, PeerTypeGoodbye,
		PeerTypeStateSummary, PeerTypeStateRequired, PeerTypeWorkspaceAttach,
		PeerTypeWorkspaceDetach, PeerTypeSessionCurrentSet, PeerTypeProviderDefinitionPut,
		PeerTypeProviderDefinitionRemove, PeerTypeProviderContextInstructionSet, PeerTypeProviderAvailability,
		PeerTypeProviderAuthChanged, PeerTypeProviderAuthInvalidated,
		PeerTypeProviderRefreshRequired, PeerTypeProviderRefreshCompleted,
		PeerTypeProviderCredentialReplace, PeerTypeProviderCredentialInvalidate,
		PeerTypeModelSelectionSet, PeerTypeModelSelectionChanged,
		PeerTypeRuntimeControlsPatch, PeerTypeRuntimePatchApplied,
		PeerTypeRuntimeTransaction, PeerTypeRuntimeReplace, PeerTypeAcknowledgement, PeerTypeError,
		PeerTypeEventLSP, PeerTypeEventMCP, PeerTypeEventPermissionRequest,
		PeerTypeEventPermissionResult, PeerTypeEventQuestionRequest,
		PeerTypeEventQuestionResult, PeerTypeEventMessage, PeerTypeEventSession,
		PeerTypeEventFile, PeerTypeEventAgent, PeerTypeEventConfigChanged,
		PeerTypeEventSkills, PeerTypeEventTask, PeerTypeRunCompleted,
		PeerTypeRunFailed, PeerTypeRunCancelled,
	}
	require.ElementsMatch(t, expected, RegisteredPeerMessageTypes())
	for _, messageType := range expected {
		spec, ok := PeerMessageSpecification(messageType)
		require.True(t, ok, messageType)
		require.NotNil(t, spec.newPayload, messageType)
		require.Positive(t, spec.MaxPayloadSize, messageType)
	}
}

func TestPeerMessageStrictDecode(t *testing.T) {
	valid, err := EncodePeerMessage(peerTestEpoch, 1, "message-1", "", "", PeerTypeHello, PeerHello{ClientID: "client-1"})
	require.NoError(t, err)

	tests := []struct {
		name      string
		data      []byte
		direction PeerMessageDirection
		want      string
	}{
		{name: "valid", data: valid, direction: PeerDirectionClientToServer},
		{name: "wrong direction", data: valid, direction: PeerDirectionServerToClient, want: "direction"},
		{name: "unknown envelope field", data: []byte(`{"version":2,"epoch":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sequence":1,"message_id":"message-1","kind":"command","type":"peer.hello","payload":{"client_id":"client-1","workspaces":[]},"extra":true}`), direction: PeerDirectionClientToServer, want: "invalid"},
		{name: "duplicate envelope field", data: []byte(`{"version":2,"version":2,"epoch":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sequence":1,"message_id":"message-1","kind":"command","type":"peer.hello","payload":{"client_id":"client-1","workspaces":[]}}`), direction: PeerDirectionClientToServer, want: "duplicate"},
		{name: "unknown payload field", data: []byte(`{"version":2,"epoch":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sequence":1,"message_id":"message-1","kind":"command","type":"peer.hello","payload":{"client_id":"client-1","workspaces":[],"extra":true}}`), direction: PeerDirectionClientToServer, want: "invalid payload"},
		{name: "unknown type", data: []byte(`{"version":2,"epoch":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sequence":1,"message_id":"message-1","kind":"command","type":"peer.other","payload":{}}`), direction: PeerDirectionClientToServer, want: "unknown"},
		{name: "workspace scope missing", data: []byte(`{"version":2,"epoch":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sequence":1,"message_id":"message-1","kind":"command","type":"workspace.detach","payload":{}}`), direction: PeerDirectionClientToServer, want: "scope"},
		{name: "connection scope has workspace", data: []byte(`{"version":2,"epoch":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sequence":1,"message_id":"message-1","kind":"command","type":"peer.hello","workspace_id":"workspace-1","payload":{"client_id":"client-1","workspaces":[]}}`), direction: PeerDirectionClientToServer, want: "scope"},
		{name: "stale version", data: []byte(`{"version":1,"epoch":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sequence":1,"message_id":"message-1","kind":"command","type":"peer.hello","payload":{"client_id":"client-1","workspaces":[]}}`), direction: PeerDirectionClientToServer, want: "version"},
		{name: "zero sequence", data: []byte(`{"version":2,"epoch":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sequence":0,"message_id":"message-1","kind":"command","type":"peer.hello","payload":{"client_id":"client-1","workspaces":[]}}`), direction: PeerDirectionClientToServer, want: "identity"},
		{name: "oversized typed payload", data: []byte(`{"version":2,"epoch":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sequence":1,"message_id":"message-1","kind":"event","type":"peer.goodbye","payload":{"reason":"` + strings.Repeat("a", 9<<10) + `"}}`), direction: PeerDirectionClientToServer, want: "registered type"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodePeerMessage(test.data, test.direction)
			if test.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestPeerMessageResponseCorrelation(t *testing.T) {
	_, err := EncodePeerMessage(peerTestEpoch, 1, "ack-1", "", "workspace-1", PeerTypeAcknowledgement, PeerAcknowledgement{Status: WorkspaceChannelStatusOK})
	require.ErrorContains(t, err, "reply_to")

	data, err := EncodePeerMessage(peerTestEpoch, 1, "ack-1", "command-1", "workspace-1", PeerTypeAcknowledgement, PeerAcknowledgement{Status: WorkspaceChannelStatusOK})
	require.NoError(t, err)
	decoded, err := DecodePeerMessage(data, PeerDirectionServerToClient)
	require.NoError(t, err)
	require.Equal(t, "command-1", decoded.Envelope.ReplyTo)
}

func TestPeerModelSelectionSetValidatesOrderedSelections(t *testing.T) {
	provider := ProviderRef{Owner: providerauth.Owner{ProviderID: "claude-ai"}, DefinitionDigest: strings.Repeat("a", 64)}
	base := PeerRuntimeBase{ExpectedRevision: 1, ExpectedDigest: strings.Repeat("b", 64), ResultRevision: 2, ResultDigest: strings.Repeat("c", 64)}
	large := PeerModelSelectionOperation{ModelType: config.SelectedModelTypeLarge, Model: ModelRef{Provider: provider, ModelID: "claude-sonnet"}, Settings: config.SelectedModel{Provider: "claude-ai", Model: "claude-sonnet"}}
	small := PeerModelSelectionOperation{ModelType: config.SelectedModelTypeSmall, Model: ModelRef{Provider: provider, ModelID: "claude-haiku"}, Settings: config.SelectedModel{Provider: "claude-ai", Model: "claude-haiku"}}

	data, err := EncodePeerMessage(peerTestEpoch, 1, "message-1", "", "workspace-1", PeerTypeModelSelectionSet, PeerModelSelectionSet{Runtime: base, Selections: []PeerModelSelectionOperation{large, small}})
	require.NoError(t, err)
	decoded, err := DecodePeerMessage(data, PeerDirectionClientToServer)
	require.NoError(t, err)
	require.Equal(t, []PeerModelSelectionOperation{large, small}, decoded.Payload.(*PeerModelSelectionSet).Selections)

	_, err = EncodePeerMessage(peerTestEpoch, 2, "message-2", "", "workspace-1", PeerTypeModelSelectionSet, PeerModelSelectionSet{Runtime: base, Selections: []PeerModelSelectionOperation{large, large}})
	require.ErrorContains(t, err, "invalid model selection")
}

func TestPeerRuntimeTransactionValidatesOrderedOperations(t *testing.T) {
	owner := providerregistry.RegistrationOwner{ProviderID: "claude-ai"}
	definition := config.RemoteProviderDefinition{Config: config.ProviderConfig{ID: owner.ProviderID}}
	digest, err := ProviderDefinitionDigest(definition)
	require.NoError(t, err)
	provider := ProviderRef{Owner: providerauth.PublicOwner(owner), DefinitionDigest: digest}
	generation := providerauth.Generation{Epoch: peerTestEpoch, Sequence: 41}
	base := PeerRuntimeBase{ExpectedRevision: 1, ExpectedDigest: strings.Repeat("b", 64), ResultRevision: 2, ResultDigest: strings.Repeat("c", 64)}
	definitionPut := PeerProviderDefinitionPut{Provider: provider, Definition: definition}
	credentialReplace := PeerProviderCredentialReplace{
		Provider:   provider,
		Generation: generation,
		Credential: config.RemoteCredentialBinding{Owner: owner, Generation: base.ResultRevision, APIKey: "secret"},
	}
	modelSelection := PeerModelSelectionOperation{
		ModelType: config.SelectedModelTypeLarge,
		Model:     ModelRef{Provider: provider, ModelID: "claude-sonnet"},
		Settings:  config.SelectedModel{Provider: owner.ProviderID, Model: "claude-sonnet"},
	}
	transaction := PeerRuntimeTransaction{Runtime: base, Operations: []PeerRuntimeOperation{
		{Type: PeerTypeProviderDefinitionPut, DefinitionPut: &definitionPut},
		{Type: PeerTypeProviderCredentialReplace, CredentialReplace: &credentialReplace},
		{Type: PeerTypeModelSelectionSet, ModelSelection: &modelSelection},
	}}

	data, err := EncodePeerMessage(peerTestEpoch, 1, "message-1", "", "workspace-1", PeerTypeRuntimeTransaction, transaction)
	require.NoError(t, err)
	decoded, err := DecodePeerMessage(data, PeerDirectionClientToServer)
	require.NoError(t, err)
	require.Equal(t, transaction, *decoded.Payload.(*PeerRuntimeTransaction))

	t.Run("out of order", func(t *testing.T) {
		invalid := transaction
		invalid.Operations = []PeerRuntimeOperation{transaction.Operations[1], transaction.Operations[0], transaction.Operations[2]}
		_, err := EncodePeerMessage(peerTestEpoch, 2, "message-2", "", "workspace-1", PeerTypeRuntimeTransaction, invalid)
		require.ErrorContains(t, err, "out of order")
	})

	t.Run("multiple typed payloads", func(t *testing.T) {
		invalid := transaction
		invalid.Operations = append([]PeerRuntimeOperation(nil), transaction.Operations...)
		invalid.Operations[0].CredentialReplace = &credentialReplace
		_, err := EncodePeerMessage(peerTestEpoch, 3, "message-3", "", "workspace-1", PeerTypeRuntimeTransaction, invalid)
		require.ErrorContains(t, err, "exactly one")
	})

	t.Run("credential runtime generation", func(t *testing.T) {
		invalidCredential := credentialReplace
		invalidCredential.Credential.Generation = generation.Sequence
		invalid := transaction
		invalid.Operations = append([]PeerRuntimeOperation(nil), transaction.Operations...)
		invalid.Operations[1].CredentialReplace = &invalidCredential
		_, err := EncodePeerMessage(peerTestEpoch, 4, "message-4", "", "workspace-1", PeerTypeRuntimeTransaction, invalid)
		require.ErrorContains(t, err, "credential replacement")
	})
}

func TestProviderAndModelReferencesAreStableAndCredentialFree(t *testing.T) {
	first := providerregistry.RegistrationOwner{ProviderID: "claude-ai", AccountNamespace: "first-private-namespace"}
	second := providerregistry.RegistrationOwner{ProviderID: "claude-ai", AccountNamespace: "second-private-namespace"}
	definition := config.RemoteProviderDefinition{Config: config.ProviderConfig{ID: "claude-ai"}}
	digest, err := ProviderDefinitionDigest(definition)
	require.NoError(t, err)

	firstRef := ProviderRef{Owner: providerauth.PublicOwner(first), DefinitionDigest: digest}
	secondRef := ProviderRef{Owner: providerauth.PublicOwner(second), DefinitionDigest: digest}
	require.Equal(t, firstRef, secondRef)
	require.NoError(t, firstRef.Validate())

	model := ModelRef{Provider: firstRef, ModelID: "claude-sonnet"}
	require.NoError(t, model.Validate())
	encoded, err := json.Marshal(model)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-namespace")
	require.NotContains(t, string(encoded), "api_key")
	require.NotContains(t, string(encoded), "token")
}
