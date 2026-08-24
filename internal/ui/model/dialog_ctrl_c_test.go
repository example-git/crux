package model

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/example-git/crux/internal/ui/completions"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

type modelsDialogStub struct{}

type instructionRefreshWorkspace struct {
	testWorkspace
	updates int
}

func (w *instructionRefreshWorkspace) UpdateAgentModel(context.Context) error {
	w.updates++
	return nil
}

func (modelsDialogStub) ID() string {
	return dialog.ModelsID
}

func (modelsDialogStub) HandleMsg(tea.Msg) dialog.Action {
	return nil
}

func (modelsDialogStub) Draw(uv.Screen, uv.Rectangle) *tea.Cursor {
	return nil
}

func TestCtrlCCloseDismissesOnboardingDialog(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.state = uiOnboarding
	ui.dialog = dialog.NewOverlay(modelsDialogStub{})

	ui.handleDialogAction(dialog.ActionClose{Dismiss: true})

	require.False(t, ui.dialog.HasDialogs())
}

func TestCtrlCClosesCompletionsBeforeClearingPrompt(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.dialog = dialog.NewOverlay()
	ui.keyMap = DefaultKeyMap()
	ui.completions = completions.New(
		ui.com.Styles.Completions.Normal,
		ui.com.Styles.Completions.Focused,
		ui.com.Styles.Completions.Match,
	)
	ui.completionsOpen = true
	ui.completionsQuery = "query"
	ui.textarea.SetValue("@query")

	ui.handleKeyPressMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})

	require.False(t, ui.completionsOpen)
	require.Empty(t, ui.completionsQuery)
	require.Equal(t, "@query", ui.textarea.Value())
	require.False(t, ui.dialog.HasDialogs())
}

func TestInstructionChangesRebuildActiveAgent(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	workspace := &instructionRefreshWorkspace{}
	ui.com.Workspace = workspace

	runCmds(ui, ui.handleDialogAction(dialog.ActionInstructionsChanged{}))

	require.Equal(t, 1, workspace.updates)
}

func TestRegularCloseKeepsRequiredOnboardingDialog(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.state = uiOnboarding
	ui.dialog = dialog.NewOverlay(modelsDialogStub{})

	ui.handleDialogAction(dialog.ActionClose{})

	require.True(t, ui.dialog.HasDialogs())
}
