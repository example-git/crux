package agent

import (
	"context"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestRunUsesPersistedSessionInstructions(t *testing.T) {
	for _, mode := range []string{"inherit", "append", "replace"} {
		for _, text := range []string{"Session-only prompt", ""} {
			t.Run(mode+"/"+text, func(t *testing.T) {
				env := testEnv(t)
				current, err := env.sessions.Create(t.Context(), "custom instructions")
				require.NoError(t, err)
				_, err = env.sessions.(session.InstructionService).SetInstructions(t.Context(), current.ID, session.Instructions{Mode: mode, Text: text})
				require.NoError(t, err)
				model := &promptCaptureModel{finishStreamModel: &finishStreamModel{text: "done"}}
				configured := Model{Model: model, SystemPromptPrefix: "Provider prefix", CatalogModel: catalog.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}}
				small := configured
				small.Model = &finishStreamModel{text: "title"}
				built := false
				tool := fantasy.NewAgentTool("fixture", "Fixture tool", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
					return fantasy.NewTextResponse("fixture"), nil
				})
				agent := NewSessionAgent(SessionAgentOptions{
					LargeModel: configured, SmallModel: small, Sessions: env.sessions, Messages: env.messages,
					SystemPrompt: "Initial native instructions", Tools: []fantasy.AgentTool{tool},
					SystemPromptBuilder: func(context.Context, session.Session, Model) (fantasy.Instructions, error) {
						built = true
						return fantasy.NewInstructions(fantasy.StaticInstruction(fantasy.InstructionKindTooling, "Rebuilt native instructions")), nil
					},
				})
				_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: "Hello", CodebaseInstructions: "Retrieved code", MemoryInstructions: "Saved memory", TurnInstructions: "Turn context"})
				require.NoError(t, err)
				require.Equal(t, 1, model.toolCount)
				system := ""
				for _, message := range model.prompt {
					if message.Role == fantasy.MessageRoleSystem {
						for _, part := range message.Content {
							if value, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
								system += value.Text + "\n"
							}
						}
					}
				}
				if mode == "replace" {
					require.False(t, built)
					if text == "" {
						require.Empty(t, system)
					} else {
						require.Equal(t, text+"\n", system)
					}
				} else {
					require.True(t, built)
					for _, inherited := range []string{"Rebuilt native instructions", "Provider prefix", "Retrieved code", "Saved memory", "Turn context"} {
						require.Contains(t, system, inherited)
					}
					if text != "" && mode == "append" {
						require.Contains(t, system, text)
					} else if text != "" {
						require.NotContains(t, system, text)
					}
				}
			})
		}
	}
}
