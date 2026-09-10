package model

import (
	"context"
	"fmt"
	"image"
	"strings"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/dialog"
)

type PreviewMenu struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

var PreviewModals = previewModalMenus()
var PreviewPopovers = []PreviewMenu{{"none", "None"}, {"files", "File mentions"}, {"resources", "MCP resources"}, {"commands", "Slash commands"}}

func (w *previewWorkspace) ListSessions(context.Context) ([]session.Session, error) {
	if w.sessions != nil {
		return w.sessions, nil
	}
	titles := []string{"TUI renderer exploration", "Provider layout review", "Compare task summaries", "Inspect compact menus", "Long output regression", "Fixture completion states"}
	sessions := []session.Session{}
	for i, title := range titles {
		sessions = append(sessions, session.Session{ID: fmt.Sprintf("fixture-session-%d", i), Title: title, CreatedAt: 1788690000 + int64(i*60), UpdatedAt: 1788690900 + int64(i*60), MessageCount: int64(12 + i), PromptTokens: int64(12000 + i*4000)})
	}
	return sessions, nil
}
func (w *previewWorkspace) ListTasks(context.Context) ([]managedtask.View, error) {
	return append([]managedtask.View(nil), w.taskData.Tasks...), nil
}

func (w *previewWorkspace) TaskOutput(_ context.Context, id string, _ bool, _ time.Duration) (managedtask.OutputResult, error) {
	for _, task := range w.taskData.Tasks {
		if task.ID == id {
			return managedtask.OutputResult{Task: task, Output: task.FinalOutput, RetrievalStatus: "success"}, nil
		}
	}
	return managedtask.OutputResult{}, fmt.Errorf("unknown fixture task %q", id)
}

func (w *previewWorkspace) ListMessages(_ context.Context, sessionID string) ([]message.Message, error) {
	for _, task := range w.taskData.Tasks {
		if task.ChildSessionID == sessionID {
			return []message.Message{{ID: task.ID + "-result", Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: task.FinalOutput}}}}, nil
		}
	}
	return nil, fmt.Errorf("unknown fixture task session %q", sessionID)
}

func (w *previewWorkspace) ListTaskNotifications(context.Context, string, bool) ([]managedtask.Notification, error) {
	return nil, nil
}

func previewTasks() []managedtask.View {
	rows := []managedtask.View{}
	types := []managedtask.Type{managedtask.TypeAgent, managedtask.TypeShell, managedtask.TypeImage}
	states := []managedtask.Status{managedtask.StatusCompleted, managedtask.StatusRunning, managedtask.StatusPending, managedtask.StatusFailed, managedtask.StatusKilled, managedtask.StatusLost}
	for i, status := range states {
		rows = append(rows, managedtask.View{ID: fmt.Sprintf("a0000000%d", i), Type: types[i%3], Description: fmt.Sprintf("Fixture %s: review layout stage %d", types[i%3], i+1), Command: "go test ./fixture", State: managedtask.State{Status: status, StartedAt: time.Unix(1788690000, 0)}, FinalOutput: previewReport("Task output"), AgentType: "explore", OutputRef: "fixture-output.log"})
		row := &rows[len(rows)-1]
		if status.Terminal() {
			row.State.EndedAt = row.State.StartedAt.Add(45 * time.Second)
			row.Usage = managedtask.AgentUsage{PromptTokens: 1200, CompletionTokens: 240, ToolUseCount: 3}
		}
		if row.Type == managedtask.TypeAgent {
			row.ChildSessionID = "fixture-child-" + row.ID
		}
	}
	return rows
}
func previewMenuValid(value string, menus []PreviewMenu) bool {
	if value == "" {
		return true
	}
	for _, m := range menus {
		if m.ID == value {
			return true
		}
	}
	return false
}

func (p *Preview) applyPreviewOverlays(o PreviewOptions) error {
	u := p.ui
	u.com.Workspace.(*previewWorkspace).taskData = p.data
	if u.taskPanel != nil {
		u.taskPanel.ClosePanel()
		u.taskPanel = nil
	}
	u.taskPanelHidden = false
	u.dialog = dialog.NewOverlay()
	u.completions.Close()
	u.completionsOpen = false
	if o.Modal != "" && o.Modal != "none" {
		var d dialog.Dialog
		var err error
		for _, entry := range previewModalRegistry {
			if entry.ID == o.Modal {
				d, err = entry.Create(p)
				break
			}
		}

		if err != nil {
			return fmt.Errorf("preview %s dialog: %w", o.Modal, err)
		}
		if d == nil {
			return fmt.Errorf("unknown preview modal %q", o.Modal)
		}
		key := tea.KeyPressMsg{Code: tea.KeyDown}
		if o.Modal == "quit" {
			key.Code = tea.KeyTab
		}
		for i := 0; i < o.MenuRow; i++ {
			d.HandleMsg(key)
		}
		if panel, ok := d.(*dialog.Tasks); ok {
			u.taskPanel = panel
			u.updateLayoutAndSize()
		} else {
			u.dialog.OpenDialog(d)
		}
	}
	if o.Popover != "" && o.Popover != "none" {
		switch o.Popover {
		case "files":
			u.completions.SetItems(p.data.FileMentions, nil)
		case "resources":
			u.completions.SetItems(nil, p.data.Resources)
		case "commands":
			u.completions.SetCommandItems(p.data.Commands)
		default:
			return fmt.Errorf("unknown preview popover %q", o.Popover)
		}
		for i := 0; i < o.MenuRow; i++ {
			u.completions.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		}
		u.completionsOpen = true
		u.completionsPositionStart = image.Pt(u.layout.editor.Min.X+3, u.layout.editor.Min.Y)
	}
	return nil
}

func (p *Preview) clickTaskPanel(point PreviewPoint, list bool) {
	m := p.ui
	if !image.Pt(point.X, point.Y).In(m.layout.editor) {
		return
	}
	cmd := m.handleTaskPanelMsg(tea.MouseClickMsg(tea.Mouse{X: point.X, Y: point.Y, Button: tea.MouseLeft}))
	if !list || cmd == nil {
		return
	}
	var apply func(tea.Cmd)
	apply = func(command tea.Cmd) {
		if command == nil {
			return
		}
		switch msg := command().(type) {
		case tea.BatchMsg:
			for _, child := range msg {
				apply(child)
			}
		case taskPanelReplyMsg:
			if m.taskPanel == msg.panel {
				m.handleTaskPanelMsg(msg.msg)
			}
		}
	}
	apply(cmd)
}

func previewOverlayNote(o PreviewOptions) string {
	if o.Modal != "" && o.Modal != "none" || o.Popover != "" && o.Popover != "none" {
		return "Fixture menu · ↑/↓ change row · Esc closes · actions are not submitted"
	}
	return strings.TrimSpace(previewExampleNote(o.Example))
}
