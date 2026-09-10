package dialog

import (
	"fmt"
	"image"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/stretchr/testify/require"
)

func TestProviderLoadIssuesScreenScrollsAndContinues(t *testing.T) {
	for _, size := range []image.Point{{X: 100, Y: 36}, {X: 40, Y: 12}, {X: 20, Y: 8}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			d := NewProviderLoadIssues(common.DefaultCommon(nil), []config.ProviderLoadIssue{{ProviderID: "codex", PluginID: "codex", Version: "1.0.0", Message: strings.Repeat("Missing endpoint bindings. ", 20)}})
			draw := func() string {
				screen := uv.NewScreenBuffer(size.X, size.Y)
				d.Draw(screen, screen.Bounds())
				return ansi.Strip(screen.Render())
			}
			view := draw()
			require.Contains(t, view, "Continue")
			require.Contains(t, view, "Providers")
			d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnd})
			require.Contains(t, draw(), "Continue")
			clicked := false
			for y := 0; y < size.Y && !clicked; y++ {
				for x := 0; x < size.X; x++ {
					if common.HitButtonIndex(d.buttonCompositor, x, y) == 0 {
						require.IsType(t, ActionContinueProviderStartup{}, d.HandleMsg(tea.MouseClickMsg{X: x, Y: y, Button: uv.MouseLeft}))
						clicked = true
						break
					}
				}
			}
			require.True(t, clicked, "Continue must remain visible and clickable")
			for _, code := range []rune{tea.KeyEnter, tea.KeyEscape} {
				require.IsType(t, ActionContinueProviderStartup{}, d.HandleMsg(tea.KeyPressMsg{Code: code}))
			}
		})
	}
}
