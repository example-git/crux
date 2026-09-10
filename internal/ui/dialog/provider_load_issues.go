package dialog

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/common"
)

const ProviderLoadIssuesID = "provider-load-issues"

type ActionContinueProviderStartup struct{}

// ProviderLoadIssues explains unavailable integrations before normal startup.
// Its body scrolls independently of the always-visible Continue button.
type ProviderLoadIssues struct {
	com               *common.Common
	issues            []config.ProviderLoadIssue
	scroll, maxScroll int
	buttonCompositor  *lipgloss.Compositor
}

func NewProviderLoadIssues(com *common.Common, issues []config.ProviderLoadIssue) *ProviderLoadIssues {
	return &ProviderLoadIssues{com: com, issues: slices.Clone(issues)}
}

func (*ProviderLoadIssues) ID() string { return ProviderLoadIssuesID }

func (d *ProviderLoadIssues) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter", "esc", "ctrl+c", " ":
			return ActionContinueProviderStartup{}
		case "up", "k":
			d.scroll--
		case "down", "j":
			d.scroll++
		case "pgup":
			d.scroll -= 5
		case "pgdown":
			d.scroll += 5
		case "home":
			d.scroll = 0
		case "end":
			d.scroll = d.maxScroll
		}
	case common.CoalescedWheelMsg:
		d.scroll -= int(msg.DeltaY)
	case tea.MouseClickMsg:
		if msg.Button == uv.MouseLeft && common.HitButtonIndex(d.buttonCompositor, msg.X, msg.Y) == 0 {
			return ActionContinueProviderStartup{}
		}
	}
	d.scroll = max(0, min(d.scroll, d.maxScroll))
	return nil
}

func (d *ProviderLoadIssues) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	frame := t.Dialog.View.Width(min(78, area.Dx()))
	inner := max(1, min(78, area.Dx())-frame.GetHorizontalFrameSize())
	title := common.DialogTitle(t, "Providers not loaded", inner, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)
	parts := []string{"Crux could not load these providers:"}
	for _, issue := range d.issues {
		name := issue.ProviderID
		if issue.PluginID != "" {
			name += fmt.Sprintf(" (%s %s)", issue.PluginID, issue.Version)
		}
		parts = append(parts, name+"\n"+issue.Message)
	}
	parts = append(parts, "Install or update the affected provider bundles, then restart Crux. Your saved accounts and model selections are kept.", "Continue to use available providers. If your selected model is unavailable, choose another model to start a conversation.")
	plain := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, ansi.Strip(strings.Join(parts, "\n\n")))
	body := lipgloss.NewStyle().Width(inner).Padding(0, min(2, max(0, (inner-1)/2))).Render(plain)
	lines := strings.Split(body, "\n")
	buttonOpts := []common.ButtonOpts{{Text: "Continue", Selected: true}}
	buttons := common.ButtonGroup(t, buttonOpts, " ")
	if lipgloss.Width(buttons) > inner {
		buttons = ansi.Truncate(buttons, inner, "")
	}
	help := ansi.Truncate("enter/esc continue · ↑/↓ scroll", inner, "")
	height := max(0, area.Dy()-frame.GetVerticalFrameSize()-lipgloss.Height(title)-lipgloss.Height(buttons)-lipgloss.Height(help)-2)
	d.maxScroll = max(0, len(lines)-height)
	d.scroll = max(0, min(d.scroll, d.maxScroll))
	visible := strings.Join(lines[d.scroll:min(len(lines), d.scroll+height)], "\n")
	view := frame.Render(strings.Join([]string{title, "", visible, "", buttons, help}, "\n"))
	center := common.CenterRect(area, min(lipgloss.Width(view), area.Dx()), min(lipgloss.Height(view), area.Dy()))
	d.buttonCompositor = common.ButtonHitCompositorForView(t, buttonOpts, view, center.Min.X, center.Min.Y)
	DrawCenter(scr, area, view)
	return nil
}
