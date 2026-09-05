package model

import (
	"context"
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/attachments"
	"github.com/example-git/crux/internal/ui/completions"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

type modelsDialogStub struct{}

type instructionsDialogStub struct{}

type resizePreviewDialogStub struct {
	resizeCount int
}

type clickActionDialogStub struct{}

type instructionRefreshWorkspace struct {
	testWorkspace
	updates int
	state   config.AgentModelState
}

func (w *instructionRefreshWorkspace) UpdateAgentModel(_ context.Context, state config.AgentModelState) error {
	w.updates++
	w.state = state
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

func (instructionsDialogStub) ID() string {
	return dialog.InstructionsID
}

func (instructionsDialogStub) HandleMsg(tea.Msg) dialog.Action {
	return nil
}

func (instructionsDialogStub) Draw(uv.Screen, uv.Rectangle) *tea.Cursor {
	return nil
}

func (*resizePreviewDialogStub) ID() string {
	return dialog.InstructionsPreviewID
}

func (d *resizePreviewDialogStub) HandleMsg(msg tea.Msg) dialog.Action {
	if _, ok := msg.(tea.WindowSizeMsg); ok {
		d.resizeCount++
	}
	return nil
}

func (*resizePreviewDialogStub) Draw(uv.Screen, uv.Rectangle) *tea.Cursor {
	return nil
}

func (clickActionDialogStub) ID() string {
	return "click-action"
}

func (clickActionDialogStub) HandleMsg(msg tea.Msg) dialog.Action {
	if msg, ok := msg.(tea.MouseClickMsg); ok && msg.Button == uv.MouseLeft {
		return dialog.ActionClose{}
	}
	return nil
}

func (clickActionDialogStub) Draw(uv.Screen, uv.Rectangle) *tea.Cursor {
	return nil
}

func TestDialogMouseClickAppliesReturnedAction(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.dialog = dialog.NewOverlay(clickActionDialogStub{})

	_, _ = ui.Update(tea.MouseClickMsg(tea.Mouse{Button: uv.MouseRight}))
	require.True(t, ui.dialog.HasDialogs())

	_, _ = ui.Update(tea.MouseClickMsg(tea.Mouse{Button: uv.MouseLeft}))
	require.False(t, ui.dialog.HasDialogs())
}

func TestInstructionPreviewReceivesWindowResize(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	preview := &resizePreviewDialogStub{}
	ui.dialog = dialog.NewOverlay(preview)

	_, _ = ui.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	require.Equal(t, 1, preview.resizeCount)
}

func TestInstructionPreviewTabChangesVisibleFormatThroughUI(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.attachments = attachments.New(nil, attachments.Keymap{})
	preview := dialog.NewInstructionsPreview(ui.com, []dialog.InstructionPreviewSection{{Content: "# Heading"}}, ui.width)
	ui.dialog = dialog.NewOverlay(preview)
	var runCommand func(tea.Cmd)
	runCommand = func(command tea.Cmd) {
		if command == nil {
			return
		}
		switch message := command().(type) {
		case nil:
		case tea.BatchMsg:
			for _, child := range message {
				runCommand(child)
			}
		default:
			_, next := ui.Update(message)
			runCommand(next)
		}
	}
	runCommand(preview.StartLoading())
	_, command := ui.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	runCommand(command)
	screen := uv.NewScreenBuffer(ui.width, ui.height)
	ui.dialog.Draw(screen, image.Rect(0, 0, ui.width, ui.height))
	if output := ansi.Strip(screen.String()); !strings.Contains(output, "Effective Instructions   Text") {
		t.Fatalf("preview did not switch to text mode: %q", output)
	}
}

func TestAsyncInstructionPreviewActionOpensPreview(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.dialog = dialog.NewOverlay(instructionsDialogStub{})

	_, cmd := ui.Update(dialog.ActionPreviewInstructions{})

	require.NotNil(t, cmd)
	require.True(t, ui.dialog.ContainsDialog(dialog.InstructionsID))
	require.True(t, ui.dialog.ContainsDialog(dialog.InstructionsPreviewID))
	require.Equal(t, dialog.InstructionsPreviewID, ui.dialog.DialogLast().ID())
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

	providerID := "copilot"
	cfg := &config.Config{
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: providerID, Model: "model"},
		},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			providerID: {
				ID: providerID,
				Owner: &config.ProviderOwnerReference{
					Type:         config.ProviderOwnerCore,
					Construction: providerregistry.ConstructionCopilot,
				},
			},
		}),
	}
	bound := config.NewTestStoreWithRegistrations(cfg, providerregistry.Integrated()...).RuntimeSnapshot().Config()
	ui := newTestUI()
	workspace := &instructionRefreshWorkspace{testWorkspace: testWorkspace{cfg: bound}}
	ui.com.Workspace = workspace

	runCmds(ui, ui.handleDialogAction(dialog.ActionInstructionsChanged{}))

	require.Equal(t, 1, workspace.updates)
	require.Equal(t, bound.AgentModelState(), workspace.state)
	require.NoError(t, workspace.state.Validate())
}

func TestRegularCloseKeepsRequiredOnboardingDialog(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.state = uiOnboarding
	ui.dialog = dialog.NewOverlay(modelsDialogStub{})

	ui.handleDialogAction(dialog.ActionClose{})

	require.True(t, ui.dialog.HasDialogs())
}
