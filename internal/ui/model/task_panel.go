package model

import (
	"image"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/dialog"
)

type taskPanelReplyMsg struct {
	panel *dialog.Tasks
	msg   tea.Msg
}

func wrapTaskPanelCmd(panel *dialog.Tasks, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			commands := make(tea.BatchMsg, len(batch))
			for i, child := range batch {
				commands[i] = wrapTaskPanelCmd(panel, child)
			}
			return commands
		}
		return taskPanelReplyMsg{panel: panel, msg: msg}
	}
}

func (m *UI) handleTaskPanelMsg(msg tea.Msg) tea.Cmd {
	if m.taskPanel == nil {
		return nil
	}
	infoHeight := len(m.taskPanel.PanelInfoLines())
	defer func() {
		if m.taskPanel != nil && len(m.taskPanel.PanelInfoLines()) != infoHeight {
			m.updateLayoutAndSize()
		}
	}()
	switch action := m.taskPanel.HandleMsg(msg).(type) {
	case dialog.ActionCmd:
		return wrapTaskPanelCmd(m.taskPanel, action.Cmd)
	case dialog.ActionClose:
		m.taskPanel.ClosePanel()
		m.taskPanel = nil
		m.focus = uiFocusEditor
		m.updateLayoutAndSize()
		if m.activeInline != nil {
			m.activeInline.SetFocused(true)
			return nil
		}
		return m.textarea.Focus()
	}
	return nil
}

func (m *UI) routeTaskPanelInput(msg tea.Msg) (bool, tea.Cmd) {
	var point image.Point
	switch event := msg.(type) {
	case tea.PasteMsg:
		return true, m.handleTaskPanelMsg(msg)
	case tea.MouseClickMsg:
		point = image.Pt(event.X, event.Y)
	case tea.MouseReleaseMsg:
		point = image.Pt(event.X, event.Y)
	case tea.MouseMotionMsg:
		point = image.Pt(event.X, event.Y)
	case tea.MouseWheelMsg:
		point = image.Pt(event.X, event.Y)
	case common.CoalescedWheelMsg:
		point = image.Pt(event.Mouse.X, event.Mouse.Y)
	default:
		return false, nil
	}
	if m.taskPanel != nil && len(m.taskPanel.PanelInfoLines()) > 0 && point.In(m.layout.pills) {
		return true, nil
	}
	if point.In(m.layout.editor) {
		return true, m.handleTaskPanelMsg(msg)
	}
	return false, nil
}
