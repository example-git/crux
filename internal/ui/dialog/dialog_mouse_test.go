package dialog

import (
	"image"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func findDialogButtonPoint(t *testing.T, compositor *lipgloss.Compositor, index, width, height int) (int, int) {
	t.Helper()
	require.NotNil(t, compositor)
	for y := range height {
		for x := range width {
			if common.HitButtonIndex(compositor, x, y) == index {
				return x, y
			}
		}
	}
	t.Fatalf("button %d has no hit target", index)
	return 0, 0
}

func TestPermissionDialogButtonsRespondToMouseClicks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		width  int
		height int
		index  int
		action PermissionAction
	}{
		{name: "allow", width: 100, height: 40, index: 0, action: PermissionAllow},
		{name: "allow session", width: 100, height: 40, index: 1, action: PermissionAllowForSession},
		{name: "deny", width: 100, height: 40, index: 2, action: PermissionDeny},
		{name: "stacked allow", width: 40, height: 24, index: 0, action: PermissionAllow},
		{name: "stacked allow session", width: 40, height: 24, index: 1, action: PermissionAllowForSession},
		{name: "stacked deny", width: 40, height: 24, index: 2, action: PermissionDeny},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			sty := styles.CharmtonePantera()
			dialog := NewPermissions(&common.Common{Styles: &sty}, permission.PermissionRequest{ID: "permission", ToolCallID: "tool", ToolName: "bash"})
			dialog.Draw(uv.NewScreenBuffer(test.width, test.height), image.Rect(0, 0, test.width, test.height))
			x, y := findDialogButtonPoint(t, dialog.buttonCompositor, test.index, test.width, test.height)

			action, ok := dialog.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: x, Y: y, Button: uv.MouseLeft})).(ActionPermissionResponse)
			require.True(t, ok)
			require.Equal(t, test.action, action.Action)
		})
	}
}

func TestQuitDialogButtonsRespondToMouseClicks(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	area := image.Rect(0, 0, 80, 24)

	confirm := NewQuit(&common.Common{Styles: &sty})
	confirm.Draw(uv.NewScreenBuffer(80, 24), area)
	x, y := findDialogButtonPoint(t, confirm.buttonCompositor, 0, 80, 24)
	require.Equal(t, ActionQuit{}, confirm.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: x, Y: y, Button: uv.MouseLeft})))

	cancel := NewQuit(&common.Common{Styles: &sty})
	cancel.Draw(uv.NewScreenBuffer(80, 24), area)
	x, y = findDialogButtonPoint(t, cancel.buttonCompositor, 1, 80, 24)
	require.Equal(t, ActionClose{}, cancel.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: x, Y: y, Button: uv.MouseLeft})))
}

func TestDialogButtonsIgnoreNonLeftClicks(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	dialog := NewQuit(&common.Common{Styles: &sty})
	dialog.Draw(uv.NewScreenBuffer(80, 24), image.Rect(0, 0, 80, 24))
	x, y := findDialogButtonPoint(t, dialog.buttonCompositor, 0, 80, 24)
	require.Nil(t, dialog.HandleMsg(tea.MouseClickMsg(tea.Mouse{X: x, Y: y, Button: uv.MouseRight})))
	require.True(t, dialog.selectedNo)
}

func TestQuitDialogButtonHoverDoesNotChangeSelection(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	dialog := NewQuit(&common.Common{Styles: &sty})
	dialog.Draw(uv.NewScreenBuffer(80, 24), image.Rect(0, 0, 80, 24))
	x, y := findDialogButtonPoint(t, dialog.buttonCompositor, 0, 80, 24)

	require.Nil(t, dialog.HandleMsg(tea.MouseMotionMsg(tea.Mouse{X: x, Y: y})))
	require.Equal(t, 1, dialog.hoveredButton)
	require.True(t, dialog.selectedNo)

	require.Nil(t, dialog.HandleMsg(tea.MouseMotionMsg(tea.Mouse{X: -1, Y: -1})))
	require.Zero(t, dialog.hoveredButton)
	require.True(t, dialog.selectedNo)
}

func TestPermissionDialogButtonHoverDoesNotChangeSelection(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	dialog := NewPermissions(&common.Common{Styles: &sty}, permission.PermissionRequest{ID: "permission", ToolCallID: "tool", ToolName: "bash"})
	dialog.Draw(uv.NewScreenBuffer(100, 40), image.Rect(0, 0, 100, 40))
	x, y := findDialogButtonPoint(t, dialog.buttonCompositor, 2, 100, 40)

	require.Nil(t, dialog.HandleMsg(tea.MouseMotionMsg(tea.Mouse{X: x, Y: y})))
	require.Equal(t, 3, dialog.hoveredButton)
	require.Zero(t, dialog.selectedOption)
	require.True(t, dialog.buttonOptions()[2].Hovered)

	require.Nil(t, dialog.HandleMsg(tea.MouseMotionMsg(tea.Mouse{X: -1, Y: -1})))
	require.Zero(t, dialog.hoveredButton)
	require.Zero(t, dialog.selectedOption)
	for _, button := range dialog.buttonOptions() {
		require.False(t, button.Hovered)
	}
}
