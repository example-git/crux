package pathtranslate

import (
	"encoding/json"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/anthropic"
	"github.com/stretchr/testify/require"
)

func TestMetadataReplayTracksProducingPolicyAndPreservesOriginal(t *testing.T) {
	store, conn := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "metadata"})
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, "/private")
	require.NoError(t, err)
	metadata := fantasy.ProviderMetadata{anthropic.Name: &anthropic.ReasoningOptionMetadata{Signature: "signed-current"}}
	reasoning := fantasy.ReasoningPart{Text: "Already translated reasoning", ProviderOptions: fantasy.ProviderOptions(metadata)}
	prompt := fantasy.Prompt{{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{reasoning}}}
	before, err := json.Marshal(prompt)
	require.NoError(t, err)
	translated, err := translator.Prompt(t.Context(), prompt)
	require.NoError(t, err)
	_, projected := fantasy.AsMessagePart[fantasy.TextPart](translated[0].Content[0])
	require.True(t, projected)
	require.NoError(t, translator.RememberMetadata(t.Context(), metadata))
	names, err = NewStore(conn).Names(t.Context())
	require.NoError(t, err)
	reopened, err := New(names, policy.Settings, "/private")
	require.NoError(t, err)
	translated, err = reopened.Prompt(t.Context(), prompt)
	require.NoError(t, err)
	require.Equal(t, reasoning, translated[0].Content[0])
	changed := policy.Settings
	changed.Targets = append(changed.Targets, Target{Path: "/private/second"})
	next, err := New(names, changed, "/private")
	require.NoError(t, err)
	translated, err = next.Prompt(t.Context(), prompt)
	require.NoError(t, err)
	_, projected = fantasy.AsMessagePart[fantasy.TextPart](translated[0].Content[0])
	require.True(t, projected)
	disabled, err := New(names, Settings{}, "/private")
	require.NoError(t, err)
	translated, err = disabled.Prompt(t.Context(), prompt)
	require.NoError(t, err)
	require.Equal(t, prompt, translated)
	after, err := json.Marshal(prompt)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

func TestMetadataReplayReconstructsPairedToolsWithoutDroppingOtherResults(t *testing.T) {
	store, _ := testStore(t)
	policy := testPolicy(t, store, Scope{Kind: "session", SessionID: "tools"})
	names, err := store.Names(t.Context())
	require.NoError(t, err)
	translator, err := New(names, policy.Settings, "/private")
	require.NoError(t, err)
	old := fantasy.ProviderOptions{anthropic.Name: &anthropic.ReasoningOptionMetadata{RedactedData: "opaque-old-state"}}
	prompt := fantasy.Prompt{
		{Role: fantasy.MessageRoleSystem, ProviderOptions: old, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "system cache", ProviderOptions: old}}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "old", ToolName: "view", Input: `{"file_path":"/private/library/space name.txt"}`, ProviderOptions: old},
			fantasy.ToolCallPart{ToolCallID: "native", ToolName: "view", Input: `{"file_path":"/public/ordinary.txt"}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "old", Output: fantasy.ToolResultOutputContentText{Text: "Read /private/library/space name.txt"}, ProviderOptions: old},
			fantasy.ToolResultPart{ToolCallID: "native", Output: fantasy.ToolResultOutputContentText{Text: "ordinary result"}},
		}},
	}
	translated, err := translator.Prompt(t.Context(), prompt)
	require.NoError(t, err)
	require.Len(t, translated, 4)
	require.Equal(t, prompt[0], translated[0])
	require.Equal(t, fantasy.MessageRoleTool, translated[2].Role)
	require.Equal(t, "native", translated[2].Content[0].(fantasy.ToolResultPart).ToolCallID)
	require.Equal(t, fantasy.MessageRoleUser, translated[3].Role)
	payload, err := json.Marshal(translated[1:])
	require.NoError(t, err)
	require.NotContains(t, string(payload), "opaque-old-state")
	require.NotContains(t, string(payload), "space name.txt")
	require.Contains(t, string(payload), "Prior tool call view (old)")
	require.Contains(t, string(payload), "Prior tool result (old)")
	require.Contains(t, string(payload), "ordinary result")
}
