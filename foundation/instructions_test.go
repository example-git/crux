package foundation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstructionsNormalizeStaticBeforeDynamic(t *testing.T) {
	instructions := NewInstructions(
		DynamicInstruction(InstructionKindMemory, "memory"),
		StaticInstruction(InstructionKindTooling, "tooling"),
		DynamicInstruction(InstructionKindRetrieval, "retrieval"),
	)

	require.Equal(t, "tooling\n\nmemory\n\nretrieval", instructions.String())
	require.Equal(t, []InstructionKind{
		InstructionKindTooling,
		InstructionKindMemory,
		InstructionKindRetrieval,
	}, []InstructionKind{
		instructions.Sections()[0].Kind,
		instructions.Sections()[1].Kind,
		instructions.Sections()[2].Kind,
	})
}

func TestInstructionsAnthropicProjectionCachesOnlyFinalStaticBlock(t *testing.T) {
	instructions := NewInstructions(
		StaticInstruction(InstructionKindProviderPrefix, "prefix"),
		StaticInstruction(InstructionKindTooling, "tooling"),
		DynamicInstruction(InstructionKindMemory, "memory"),
		DynamicInstruction(InstructionKindRetrieval, "retrieval"),
	)

	message := instructions.Message(InstructionPolicyAnthropic)
	require.Len(t, message.Content, 4)

	prefixPart, ok := AsMessagePart[TextPart](message.Content[0])
	require.True(t, ok)
	require.Equal(t, "prefix", prefixPart.Text)
	require.False(t, InstructionPartOptionsFrom(prefixPart.ProviderOptions).CacheBoundary)

	staticPart, ok := AsMessagePart[TextPart](message.Content[1])
	require.True(t, ok)
	require.Equal(t, "tooling", staticPart.Text)
	staticOptions := InstructionPartOptionsFrom(staticPart.ProviderOptions)
	require.NotNil(t, staticOptions)
	require.Equal(t, InstructionStabilityStatic, staticOptions.Stability)
	require.True(t, staticOptions.CacheBoundary)

	memoryPart, ok := AsMessagePart[TextPart](message.Content[2])
	require.True(t, ok)
	require.Equal(t, "memory", memoryPart.Text)
	require.Equal(t, InstructionStabilityDynamic, InstructionPartOptionsFrom(memoryPart.ProviderOptions).Stability)
	require.False(t, InstructionPartOptionsFrom(memoryPart.ProviderOptions).CacheBoundary)

	retrievalPart, ok := AsMessagePart[TextPart](message.Content[3])
	require.True(t, ok)
	require.Equal(t, "retrieval", retrievalPart.Text)
	require.Equal(t, InstructionStabilityDynamic, InstructionPartOptionsFrom(retrievalPart.ProviderOptions).Stability)
	require.False(t, InstructionPartOptionsFrom(retrievalPart.ProviderOptions).CacheBoundary)
}

func TestInstructionsGenericProjectionDoesNotCreateCacheBoundary(t *testing.T) {
	message := NewInstructions(
		StaticInstruction(InstructionKindTooling, "tooling"),
	).Message(InstructionPolicyGeneric)

	require.Len(t, message.Content, 1)
	part, ok := AsMessagePart[TextPart](message.Content[0])
	require.True(t, ok)
	require.False(t, InstructionPartOptionsFrom(part.ProviderOptions).CacheBoundary)
}

func TestAppendDynamicInstructionPreservesExistingParts(t *testing.T) {
	original := NewInstructions(
		StaticInstruction(InstructionKindTooling, "tooling"),
		DynamicInstruction(InstructionKindProviderContext, "provider context"),
	).Message(InstructionPolicyAnthropic)
	prompt := Prompt{original, NewUserMessage("prompt")}

	result := AppendDynamicInstruction(prompt, InstructionKindSchema, "schema")

	require.Len(t, result, 2)
	require.Len(t, result[0].Content, 3)
	require.Len(t, prompt[0].Content, 2)

	staticPart, ok := AsMessagePart[TextPart](result[0].Content[0])
	require.True(t, ok)
	require.True(t, InstructionPartOptionsFrom(staticPart.ProviderOptions).CacheBoundary)

	providerPart, ok := AsMessagePart[TextPart](result[0].Content[1])
	require.True(t, ok)
	require.Equal(t, InstructionKindProviderContext, InstructionPartOptionsFrom(providerPart.ProviderOptions).Kinds[0])
	require.False(t, InstructionPartOptionsFrom(providerPart.ProviderOptions).CacheBoundary)

	schemaPart, ok := AsMessagePart[TextPart](result[0].Content[2])
	require.True(t, ok)
	schemaOptions := InstructionPartOptionsFrom(schemaPart.ProviderOptions)
	require.Equal(t, []InstructionKind{InstructionKindSchema}, schemaOptions.Kinds)
	require.Equal(t, InstructionStabilityDynamic, schemaOptions.Stability)
	require.False(t, schemaOptions.CacheBoundary)
}

func TestAppendDynamicInstructionCreatesSystemMessage(t *testing.T) {
	prompt := Prompt{NewUserMessage("prompt")}

	result := AppendDynamicInstruction(prompt, InstructionKindSchema, "schema")

	require.Len(t, result, 2)
	require.Equal(t, MessageRoleSystem, result[0].Role)
	require.Equal(t, MessageRoleUser, result[1].Role)
}

func TestInstructionPartOptionsJSONRoundTrip(t *testing.T) {
	original := TextPart{
		Text: "tooling",
		ProviderOptions: ProviderOptions{
			InstructionOptionsKey: &InstructionPartOptions{
				Kinds:         []InstructionKind{InstructionKindTooling},
				Stability:     InstructionStabilityStatic,
				CacheBoundary: true,
			},
		},
	}

	data, err := json.Marshal(original)
	require.NoError(t, err)
	var decoded TextPart
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, original.Text, decoded.Text)
	require.Equal(t, InstructionPartOptionsFrom(original.ProviderOptions), InstructionPartOptionsFrom(decoded.ProviderOptions))
}
