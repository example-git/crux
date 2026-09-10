package model

import (
	"context"
	"errors"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/dialog"
	"image/color"
	"testing"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestDeliveryBadgesPreservePermissionAndPlanningModes(t *testing.T) {
	for _, test := range []struct {
		label      string
		mode       agent.DeliveryMode
		yolo       bool
		plan       bool
		background color.NRGBA
	}{
		{"Q", agent.DeliveryQueue, false, false, color.NRGBA{0xBF, 0xBC, 0xC8, 0xFF}},
		{"S", agent.DeliverySteer, false, false, color.NRGBA{0xF0, 0x71, 0x78, 0xFF}},
		{"Yq", agent.DeliveryQueue, true, false, color.NRGBA{0xF5, 0xD6, 0x7B, 0xFF}},
		{"Ys", agent.DeliverySteer, true, false, color.NRGBA{0xF5, 0xA3, 0x5C, 0xFF}},
		{"Pq", agent.DeliveryQueue, true, true, color.NRGBA{0x7A, 0xA2, 0xF7, 0xFF}},
		{"Ps", agent.DeliverySteer, true, true, color.NRGBA{0xBB, 0x9A, 0xF7, 0xFF}},
	} {
		t.Run(test.label, func(t *testing.T) {
			m := newBusyUI(&countingWorkspace{ready: true, yolo: test.yolo})
			m.session = &session.Session{ID: "session"}
			if test.plan {
				m.session.Mode = session.ModePlan
			}
			m.deliveryMode = test.mode
			m.textarea.Focus()
			m.textarea.SetWidth(40)
			m.setEditorPrompt(test.yolo)
			require.Contains(t, ansi.Strip(m.textarea.View()), test.label)
			require.Equal(t, test.background, color.NRGBAModel.Convert(m.com.Styles.Editor.DeliveryBadges[test.label].GetBackground()))
			before := m.session.Mode
			m.toggleDeliveryMode()
			require.NotEqual(t, test.mode, m.deliveryMode)
			require.Equal(t, before, m.session.Mode)
			m.toggleDeliveryMode()
			require.Equal(t, test.mode, m.deliveryMode)
		})
	}
}

type deliveryWorkspace struct {
	*countingWorkspace
	modes []agent.DeliveryMode
}

func (w *deliveryWorkspace) AgentRun(ctx context.Context, _ string, _ string, _ ...message.Attachment) error {
	w.modes = append(w.modes, agent.DeliveryModeFromContext(ctx))
	return nil
}

func TestSubmissionCapturesDeliveryModeWhileBusy(t *testing.T) {
	for _, mode := range []agent.DeliveryMode{agent.DeliveryQueue, agent.DeliverySteer} {
		t.Run(string(mode), func(t *testing.T) {
			ws := &deliveryWorkspace{countingWorkspace: &countingWorkspace{ready: true, agentBusy: true}}
			m := newBusyUI(ws.countingWorkspace)
			m.com.Workspace = ws
			m.session = &session.Session{ID: "session"}
			m.deliveryMode = mode
			cmd := m.sendMessage("new direction")
			m.toggleDeliveryMode()
			var execute func(tea.Cmd)
			execute = func(cmd tea.Cmd) {
				if cmd == nil {
					return
				}
				if batch, ok := cmd().(tea.BatchMsg); ok {
					for _, next := range batch {
						execute(next)
					}
				}
			}
			execute(cmd)
			require.Equal(t, []agent.DeliveryMode{mode}, ws.modes)
		})
	}
}

type deliveryPreferenceWorkspace struct {
	*countingWorkspace
	values []string
	err    error
}

func (w *deliveryPreferenceWorkspace) SetConfigField(scope config.Scope, key string, value any) error {
	if scope != config.ScopeGlobal || key != "options.tui.delivery_mode" {
		return errors.New("unexpected preference destination")
	}
	w.values = append(w.values, value.(string))
	return w.err
}

func TestDeliveryPreferenceSaveOrderingAndQuit(t *testing.T) {
	ws := &deliveryPreferenceWorkspace{countingWorkspace: &countingWorkspace{ready: true}}
	m := newBusyUI(ws.countingWorkspace)
	m.com.Workspace = ws
	first := m.toggleDeliveryMode()
	second := m.toggleDeliveryMode()
	quit := m.handleDialogAction(dialog.ActionQuit{})
	secondDone := make(chan struct{})
	go func() {
		second()
		close(secondDone)
	}()
	quitDone := make(chan tea.Msg, 1)
	go func() { quitDone <- quit() }()
	select {
	case <-secondDone:
		t.Fatal("second save bypassed the first")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-quitDone:
		t.Fatal("quit bypassed pending saves")
	default:
	}
	require.Nil(t, first())
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second save did not finish")
	}
	select {
	case msg := <-quitDone:
		require.IsType(t, tea.QuitMsg{}, msg)
	case <-time.After(5 * time.Second):
		t.Fatal("quit did not finish")
	}
	require.Equal(t, []string{"steer", "queue"}, ws.values)
	ws.err = errors.New("disk unavailable")
	require.NotNil(t, m.toggleDeliveryMode()())
}

func TestDeliveryPreferenceConstructor(t *testing.T) {
	for _, mode := range []string{"", "queue", "steer"} {
		p, err := NewPreview()
		require.NoError(t, err)
		_, err = p.Render(PreviewOptions{Example: "tool-bash", Model: "dummy-coder", Scenario: "working", Cols: 120, Rows: 40})
		require.NoError(t, err)
		p.ui.com.Config().Options.TUI.DeliveryMode = mode
		m := New(p.ui.com, "", false, "")
		expected := agent.DeliveryQueue
		if mode == "steer" {
			expected = agent.DeliverySteer
		}
		require.Equal(t, expected, m.deliveryMode)
	}
}

func TestPendingSteeringIsNotLabeledQueued(t *testing.T) {
	m := newBusyUI(&countingWorkspace{ready: true})
	steer := agent.QueuedPrompt{DeliveryMode: agent.DeliverySteer, Prompt: "redirect now"}
	view := ansi.Strip(queuePill(1, m.com.Styles, steer))
	require.Contains(t, view, "1 Steering")
	require.NotContains(t, view, "Queued")
	require.Contains(t, ansi.Strip(queueList([]agent.QueuedPrompt{steer}, m.com.Styles)), "Steer: redirect now")
	mixed := ansi.Strip(queuePill(2, m.com.Styles, agent.QueuedPrompt{DeliveryMode: agent.DeliveryQueue}, steer))
	require.Contains(t, mixed, "1 Queued")
	require.Contains(t, mixed, "1 Steering")
}
