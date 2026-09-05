package agent

import (
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/stretchr/testify/require"
)

func TestInstructionSnapshotSectionsUseEmittedCacheBoundary(t *testing.T) {
	instructions := fantasy.NewInstructions(
		fantasy.StaticInstruction(fantasy.InstructionKindTooling, "tooling"),
		fantasy.StaticInstruction(fantasy.InstructionKindEnvironment, "environment"),
		fantasy.DynamicInstruction(fantasy.InstructionKindProviderContext, "provider context"),
	)

	sections := instructionSnapshotSections(instructions, fantasy.InstructionPolicyAnthropic)
	require.Len(t, sections, 3)
	require.False(t, sections[0].CacheBoundary)
	require.True(t, sections[1].CacheBoundary)
	require.False(t, sections[2].CacheBoundary)
	require.Equal(t, fantasy.InstructionStabilityDynamic, sections[2].Stability)
	require.Equal(t, "provider context", sections[2].Text)
}

func TestInstructionSnapshotSectionsGenericHasNoCacheBoundary(t *testing.T) {
	instructions := fantasy.NewInstructions(
		fantasy.StaticInstruction(fantasy.InstructionKindTooling, "tooling"),
	)

	sections := instructionSnapshotSections(instructions, fantasy.InstructionPolicyGeneric)
	require.Len(t, sections, 1)
	require.False(t, sections[0].CacheBoundary)
}
