package model

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/foundation/bubbles/help"
	"github.com/example-git/crux/foundation/bubbles/key"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
)

// DefaultStatusTTL is the default time-to-live for status messages.
const DefaultStatusTTL = 5 * time.Second

// Status is the status bar and help model.
type Status struct {
	com      *common.Common
	hideHelp bool
	help     help.Model
	helpKm   help.KeyMap
	msg      util.InfoMsg
}

// NewStatus creates a new status bar and help model.
func NewStatus(com *common.Common, km help.KeyMap) *Status {
	s := new(Status)
	s.com = com
	s.help = help.New()
	s.help.Styles = com.Styles.Help
	s.helpKm = km
	return s
}

// SetInfoMsg sets the status info message.
func (s *Status) SetInfoMsg(msg util.InfoMsg) {
	s.msg = msg
}

// ClearInfoMsg clears the status info message.
func (s *Status) ClearInfoMsg() {
	s.msg = util.InfoMsg{}
}

// SetWidth sets the width of the status bar and help view.
func (s *Status) SetWidth(width int) {
	helpStyle := s.com.Styles.Status.Help
	horizontalPadding := helpStyle.GetPaddingLeft() + helpStyle.GetPaddingRight()
	s.help.SetWidth(width - horizontalPadding)
}

// ShowingAll returns whether the full help view is shown.
func (s *Status) ShowingAll() bool {
	return s.help.ShowAll
}

// ToggleHelp toggles the full help view.
func (s *Status) ToggleHelp() {
	s.help.ShowAll = !s.help.ShowAll
}

// SetHideHelp sets whether the app is on the onboarding flow.
func (s *Status) SetHideHelp(hideHelp bool) {
	s.hideHelp = hideHelp
}

// Draw draws the status bar onto the screen.
func (s *Status) Draw(scr uv.Screen, area uv.Rectangle) {
	if !s.hideHelp && s.helpKm != nil {
		visible := area.Intersect(scr.Bounds())
		style := s.com.Styles.Status.Help
		width := max(0, visible.Dx()-style.GetHorizontalFrameSize())
		s.help.SetWidth(width)
		view := s.help.View(s.helpKm)
		if !s.help.ShowAll {
			view = s.renderShortHelp(width)
		}
		helpView := style.Render(view)
		uv.NewStyledString(helpView).Draw(scr, visible)
	}

	visibleArea := area.Intersect(scr.Bounds())
	info := s.renderInfo(visibleArea.Dx())
	if info == "" {
		return
	}
	uv.NewStyledString(info).Draw(scr, visibleArea)
}

func (s *Status) renderShortHelp(width int) string {
	if width <= 0 || s.helpKm == nil {
		return ""
	}
	bindings := s.helpKm.ShortHelp()
	if ansi.StringWidth(dialog.ShortHelpLine(&s.help, bindings, 1<<20)) > width {
		ordered := make([]key.Binding, 0, len(bindings))
		for priority := 0; priority < 4; priority++ {
			for _, binding := range bindings {
				rank := 3
				switch binding.Help().Desc {
				case "commands":
					rank = 1
				case "help", "more":
					rank = 2
				}
				for _, name := range binding.Keys() {
					if name == "esc" || name == "ctrl+b" {
						rank = 0
					}
				}
				if rank == priority {
					ordered = append(ordered, binding)
				}
			}
		}
		bindings = ordered
	}
	bindings = append([]key.Binding(nil), bindings...)
	for _, group := range s.helpKm.FullHelp() {
		bindings = append(bindings, group...)
	}
	seen := make(map[string]bool)
	unique := make([]key.Binding, 0, len(bindings))
	for _, binding := range bindings {
		identity := strings.Join(binding.Keys(), "\x00")
		if binding.Enabled() && !seen[identity] {
			seen[identity] = true
			unique = append(unique, binding)
		}
	}
	budget := width
	tail := " " + s.help.Styles.Ellipsis.Inline(true).Render("…")
	if ansi.StringWidth(dialog.ShortHelpLine(&s.help, unique, 1<<20)) > width {
		budget = max(0, width-ansi.StringWidth(tail))
	}
	selected := make([]key.Binding, 0, len(unique))
	omitted := false
	for _, binding := range unique {
		candidate := append(selected, binding)
		if ansi.StringWidth(dialog.ShortHelpLine(&s.help, candidate, 1<<20)) > budget {
			omitted = true
			continue
		}
		selected = candidate
	}
	view := dialog.ShortHelpLine(&s.help, selected, width)
	if omitted {
		if ansi.StringWidth(view)+ansi.StringWidth(tail) <= width {
			view += tail
		}
	}
	return view
}

func (s *Status) renderInfo(width int) string {
	if width <= 0 || s.msg.IsEmpty() {
		return ""
	}

	var indicatorStyle lipgloss.Style
	var messageStyle lipgloss.Style
	switch s.msg.Type {
	case util.InfoTypeError:
		indicatorStyle = s.com.Styles.Status.ErrorIndicator
		messageStyle = s.com.Styles.Status.ErrorMessage
	case util.InfoTypeWarn:
		indicatorStyle = s.com.Styles.Status.WarnIndicator
		messageStyle = s.com.Styles.Status.WarnMessage
	case util.InfoTypeUpdate:
		indicatorStyle = s.com.Styles.Status.UpdateIndicator
		messageStyle = s.com.Styles.Status.UpdateMessage
	case util.InfoTypeInfo:
		indicatorStyle = s.com.Styles.Status.InfoIndicator
		messageStyle = s.com.Styles.Status.InfoMessage
	case util.InfoTypeSuccess:
		indicatorStyle = s.com.Styles.Status.SuccessIndicator
		messageStyle = s.com.Styles.Status.SuccessMessage
	}

	indicator := indicatorStyle.String()
	indicatorWidth := lipgloss.Width(indicator)
	if indicatorWidth >= width {
		return ansi.Truncate(indicator, width, "")
	}

	remaining := width - indicatorWidth
	messageFrame := messageStyle.GetHorizontalFrameSize()
	if remaining <= messageFrame {
		return indicator
	}

	messageWidth := remaining - messageFrame
	message := strings.ReplaceAll(s.msg.Msg, "\n", " ")
	message = ansi.Truncate(message, messageWidth, "…")
	if message == "" {
		return indicator
	}
	if width := lipgloss.Width(message); width < messageWidth {
		message += strings.Repeat(" ", messageWidth-width)
	}
	return indicator + messageStyle.Render(message)
}

// clearInfoMsgCmd returns a command that clears the info message after the
// given TTL.
func clearInfoMsgCmd(ttl time.Duration) tea.Cmd {
	return tea.Tick(ttl, func(time.Time) tea.Msg {
		return util.ClearStatusMsg{}
	})
}
