package agent

import (
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/stretchr/testify/require"
)

func TestSubagentInstructionsReuseMainStablePrefix(t *testing.T) {
	main := fantasy.NewInstructions(
		fantasy.StaticInstruction(fantasy.InstructionKindProviderPrefix, "provider prefix"),
		fantasy.StaticInstruction(fantasy.InstructionKindTooling, "stable tooling"),
		fantasy.StaticInstruction(fantasy.InstructionKindEnvironment, "stable environment"),
		fantasy.DynamicInstruction(fantasy.InstructionKindProviderContext, "provider context"),
		fantasy.DynamicInstruction(fantasy.InstructionKindLifecycle, "lifecycle"),
		fantasy.DynamicInstruction(fantasy.InstructionKindMemory, "memory"),
		fantasy.DynamicInstruction(fantasy.InstructionKindMCP, "mcp"),
	)
	subagent := applySubagentInstructions(main, true, "custom role")

	var mainStatic []fantasy.InstructionSection
	for _, section := range main.Sections() {
		if section.Stability == fantasy.InstructionStabilityStatic {
			mainStatic = append(mainStatic, section)
		}
	}
	var subagentStatic []fantasy.InstructionSection
	var subagentDynamic []fantasy.InstructionSection
	for _, section := range subagent.Sections() {
		if section.Stability == fantasy.InstructionStabilityStatic {
			subagentStatic = append(subagentStatic, section)
		} else {
			subagentDynamic = append(subagentDynamic, section)
		}
	}
	require.Equal(t, mainStatic, subagentStatic)
	require.Equal(t, []fantasy.InstructionKind{
		fantasy.InstructionKindProviderContext,
		fantasy.InstructionKindTooling,
		fantasy.InstructionKindAuxiliary,
	}, []fantasy.InstructionKind{
		subagentDynamic[0].Kind,
		subagentDynamic[1].Kind,
		subagentDynamic[2].Kind,
	})
	require.Equal(t, "provider context", subagentDynamic[0].Text)
	require.Equal(t, subagentPolicyInstructions, subagentDynamic[1].Text)
	require.Equal(t, "custom role", subagentDynamic[2].Text)
	require.NotContains(t, subagent.String(), "lifecycle")
	require.NotContains(t, subagent.String(), "memory")
	require.NotContains(t, subagent.String(), "mcp")
	require.Equal(t, instructionCacheBoundaryText(t, main), instructionCacheBoundaryText(t, subagent))
}

func TestMainInstructionsRemainUnchanged(t *testing.T) {
	main := fantasy.NewInstructions(
		fantasy.StaticInstruction(fantasy.InstructionKindTooling, "stable"),
		fantasy.DynamicInstruction(fantasy.InstructionKindMemory, "memory"),
	)
	require.Equal(t, main.Sections(), applySubagentInstructions(main, false, "ignored").Sections())
}

func instructionCacheBoundaryText(t *testing.T, instructions fantasy.Instructions) string {
	t.Helper()
	var boundary string
	for _, part := range instructions.Message(fantasy.InstructionPolicyAnthropic).Content {
		text, ok := part.(fantasy.TextPart)
		if !ok {
			continue
		}
		options := fantasy.InstructionPartOptionsFrom(text.ProviderOptions)
		if options == nil || !options.CacheBoundary {
			continue
		}
		require.Empty(t, boundary)
		boundary = text.Text
	}
	require.NotEmpty(t, boundary)
	return boundary
}
