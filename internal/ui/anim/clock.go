package anim

import (
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
)

type FrameMsg struct {
	generation uint64
}

type Clock struct {
	pending    map[string]StepMsg
	spare      map[string]StepMsg
	generation uint64
	armed      bool
}

func (c *Clock) Add(step StepMsg) tea.Cmd {
	if c.pending == nil {
		c.pending = make(map[string]StepMsg)
	}
	if previous, ok := c.pending[step.ID]; !ok || step.instance > previous.instance || (step.instance == previous.instance && step.Gen >= previous.Gen) {
		c.pending[step.ID] = step
	}
	return c.arm()
}

func (c *Clock) arm() tea.Cmd {
	if c.armed || len(c.pending) == 0 {
		return nil
	}
	c.armed = true
	c.generation++
	generation := c.generation
	return tea.Tick(time.Second/time.Duration(fps), func(time.Time) tea.Msg {
		return FrameMsg{generation: generation}
	})
}

func (c *Clock) Advance(frame FrameMsg, advance func(StepMsg) tea.Cmd) tea.Cmd {
	if !c.armed || frame.generation != c.generation {
		return nil
	}
	c.armed = false
	pending := c.pending
	c.pending = c.spare
	if c.pending == nil {
		c.pending = make(map[string]StepMsg, len(pending))
	}
	for _, step := range pending {
		if next := advance(step); next != nil {
			if step, ok := next().(StepMsg); ok {
				c.pending[step.ID] = step
			}
		}
	}
	clear(pending)
	c.spare = pending
	return c.arm()
}

func (c *Clock) Next(frame FrameMsg) tea.Cmd {
	if !c.armed || frame.generation != c.generation {
		return nil
	}
	c.armed = false
	return c.arm()
}
