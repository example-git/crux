package proto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMessagePreservesCodexTurnInputs(t *testing.T) {
	original := Message{
		ID: "message", Role: User, SessionID: "session",
		Parts: []ContentPart{
			TextContent{Text: "prompt", TextElements: []any{"mention", map[string]any{"type": "reference", "value": "file.go"}}},
			SkillContent{Name: "fixture", Path: "/skills/fixture/SKILL.md", Content: []byte("skill body")},
		},
	}
	encoded, err := json.Marshal(original)
	require.NoError(t, err)
	var decoded Message
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, original, decoded)
}

func TestAgentMessageInputPartsPreserveNilAndExplicitEmpty(t *testing.T) {
	nilEncoded, err := json.Marshal(AgentMessage{SessionID: "session"})
	require.NoError(t, err)
	require.JSONEq(t, `{"session_id":"session","prompt":"","input_parts":null}`, string(nilEncoded))

	emptyEncoded, err := json.Marshal(AgentMessage{SessionID: "session", InputParts: []AgentInputPart{}})
	require.NoError(t, err)
	require.JSONEq(t, `{"session_id":"session","prompt":"","input_parts":[]}`, string(emptyEncoded))

	var nilDecoded AgentMessage
	require.NoError(t, json.Unmarshal(nilEncoded, &nilDecoded))
	require.Nil(t, nilDecoded.InputParts)

	var emptyDecoded AgentMessage
	require.NoError(t, json.Unmarshal(emptyEncoded, &emptyDecoded))
	require.NotNil(t, emptyDecoded.InputParts)
	require.Empty(t, emptyDecoded.InputParts)
}
