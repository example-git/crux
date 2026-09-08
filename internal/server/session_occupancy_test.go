package server

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSessionProtocolIncludesCompactionOccupancy(t *testing.T) {
	current := session.Session{ID: "session", PromptTokens: 100000, CompletionTokens: 1000, UnseenLocalTokens: 35000}
	encoded, err := json.Marshal(sessionToProto(current))
	require.NoError(t, err)
	var decoded proto.Session
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, current.PromptTokens, decoded.PromptTokens)
	require.Equal(t, current.CompletionTokens, decoded.CompletionTokens)
	require.Equal(t, current.UnseenLocalTokens, decoded.UnseenLocalTokens)
	require.Equal(t, current.ContextTokens(), decoded.PromptTokens+decoded.CompletionTokens+decoded.UnseenLocalTokens)
}
