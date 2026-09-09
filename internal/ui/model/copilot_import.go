package model

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/ui/dialog"
)

type copilotImportDoneMsg struct {
	selection  dialog.ActionSelectModel
	generation uint64
	err        error
}

// Import runs outside Update. Completion re-enters model selection using the
// acknowledged config, and only while this is still the requested selection.
func (m *UI) importCopilotCmd(selection dialog.ActionSelectModel) tea.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	m.cancelCopilotImport = cancel
	generation, workspace := m.modelSelectionGen, m.com.Workspace
	return func() tea.Msg {
		defer cancel()
		_, err := workspace.ImportCopilot(ctx, selection.ProviderOwner)
		return copilotImportDoneMsg{selection: selection, generation: generation, err: err}
	}
}
