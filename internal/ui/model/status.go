package model

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/ui/common"
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
	if !s.hideHelp {
		helpView := s.com.Styles.Status.Help.Render(s.help.View(s.helpKm))
		uv.NewStyledString(helpView).Draw(scr, area)
	}

	visibleArea := area.Intersect(scr.Bounds())
	info := s.renderInfo(visibleArea.Dx())
	if info == "" {
		return
	}
	uv.NewStyledString(info).Draw(scr, visibleArea)
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
