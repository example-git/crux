package message

import (
	"encoding/json"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/stretchr/testify/require"
)

func TestScratchCodexCompactRoundtrip(t *testing.T) {
	history := &responses.CompactedHistory{}
	require.NoError(t, json.Unmarshal([]byte(`{"items":[{"type":"message","role":"user","content":[{"type":"input_text","text":"handoff summary"}]}]}`), history))
	envelope, err := NewProviderMetadataValue(responses.Name, 1, ProviderMetadataScopeCompaction, history)
	require.NoError(t, err)
	tc := TextContent{
		Text:             "detailed summary report",
		ProviderMetadata: ProviderMetadata{envelope},
	}
	data, err := marshalParts([]ContentPart{tc, Finish{Reason: FinishReasonEndTurn}})
	require.NoError(t, err)

	parts, err := unmarshalParts(data)
	require.NoError(t, err)

	msg := Message{Role: User, Parts: parts}
	require.Len(t, msg.Content().ProviderMetadata, 1, "opaque compacted history lost in DB roundtrip")

	aiMsgs := msg.ToAIMessage()
	require.Len(t, aiMsgs, 1)
	var found bool
	for _, p := range aiMsgs[0].Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			if v, ok := tp.ProviderOptions[responses.Name]; ok {
				if ch, ok := v.(*responses.CompactedHistory); ok {
					found = true
					data, _ := json.Marshal(struct {
						Items []json.RawMessage `json:"items"`
					}{})
					_ = data
					raw, err := json.Marshal(ch)
					require.NoError(t, err)
					t.Logf("attached history: %s", raw)
					require.Contains(t, string(raw), "handoff summary", "attached history lost its items")
				}
			}
		}
	}
	require.True(t, found, "compacted history not attached to outgoing text part")
}
