package anim

import (
	"testing"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/stretchr/testify/require"
)

func TestClockCoalescesAnimationsAndStops(t *testing.T) {
	var clock Clock
	first := New(Settings{ID: "first", Size: 5})
	second := New(Settings{ID: "second", Size: 5})
	require.NotNil(t, clock.Add(first.Start()().(StepMsg)))
	require.Nil(t, clock.Add(second.Start()().(StepMsg)))
	advance := func(step StepMsg) tea.Cmd {
		if step.ID == "first" {
			return first.Animate(step)
		}
		return second.Animate(step)
	}
	frame := FrameMsg{generation: clock.generation}
	require.NotNil(t, clock.Advance(frame, advance))
	require.EqualValues(t, 1, first.framesSinceStart.Load())
	require.EqualValues(t, 1, second.framesSinceStart.Load())
	require.Nil(t, clock.Advance(frame, advance))
	require.EqualValues(t, 1, first.framesSinceStart.Load())
	first.Stop()
	second.Stop()
	require.Nil(t, clock.Advance(FrameMsg{generation: clock.generation}, advance))
	require.Empty(t, clock.pending)
	require.False(t, clock.armed)
}

func TestClockKeepsNewestGenerationAndPauses(t *testing.T) {
	var clock Clock
	animation := New(Settings{ID: "same", Size: 5})
	old := animation.Start()().(StepMsg)
	current := animation.Start()().(StepMsg)
	require.NotNil(t, clock.Add(current))
	require.Nil(t, clock.Add(old))
	require.Equal(t, current, clock.pending[current.ID])
	require.NotNil(t, clock.Next(FrameMsg{generation: clock.generation}))
	require.Zero(t, animation.framesSinceStart.Load())
	require.Nil(t, clock.Advance(FrameMsg{generation: clock.generation}, func(StepMsg) tea.Cmd { return nil }))
	require.Empty(t, clock.pending)
}

func TestClockReplacementRejectsOldInstance(t *testing.T) {
	var clock Clock
	old := New(Settings{ID: "tool", Size: 5})
	old.Start()
	oldStep := old.Start()().(StepMsg)
	current := New(Settings{ID: "tool", Size: 5})
	currentStep := current.Start()().(StepMsg)
	clock.Add(oldStep)
	clock.Add(currentStep)
	clock.Add(oldStep)
	require.Equal(t, currentStep, clock.pending["tool"])
}
