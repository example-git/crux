package dialog

import (
	"slices"
	"strings"
	"time"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/ui/common"
)

const (
	ErrorsID = "errors"

	// MaxRecentErrors bounds the error history retained by the UI.
	MaxRecentErrors = 50
)

// ErrorRecord is one full-length error message reported to the status bar
// or rendered as an error banner in the chat. Key, when set, identifies the
// source so repeated updates replace the earlier record instead of
// duplicating it.
type ErrorRecord struct {
	Time    time.Time
	Message string
	Key     string
}

// Errors shows the complete text of recently reported errors so that a
// message truncated in the status line can be read and copied.
type Errors struct {
	com               *common.Common
	records           []ErrorRecord
	scroll, maxScroll int
}

func NewErrors(com *common.Common, records []ErrorRecord) *Errors {
	return &Errors{com: com, records: slices.Clone(records)}
}

func (*Errors) ID() string { return ErrorsID }

// SetRecords replaces the displayed history while the dialog stays open.
func (d *Errors) SetRecords(records []ErrorRecord) {
	d.records = slices.Clone(records)
}

func (d *Errors) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc", "ctrl+c", "q":
			return ActionClose{}
		case "c", "enter":
			if latest := d.Latest(); latest != "" {
				return ActionCmd{common.CopyToClipboard(latest, "Latest error copied to clipboard")}
			}
		case "a":
			if all := d.all(); all != "" {
				return ActionCmd{common.CopyToClipboard(all, "All errors copied to clipboard")}
			}
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
	}
	d.scroll = max(0, min(d.scroll, d.maxScroll))
	return nil
}

// Latest returns the newest recorded error text.
func (d *Errors) Latest() string {
	if len(d.records) == 0 {
		return ""
	}
	return d.records[len(d.records)-1].Message
}

func (d *Errors) all() string {
	parts := make([]string, 0, len(d.records))
	for i := len(d.records) - 1; i >= 0; i-- {
		record := d.records[i]
		parts = append(parts, record.Time.Format(time.DateTime)+"\n"+record.Message)
	}
	return strings.Join(parts, "\n\n")
}

func (d *Errors) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	frame := t.Dialog.View.Width(min(100, area.Dx()))
	inner := max(1, min(100, area.Dx())-frame.GetHorizontalFrameSize())
	title := common.DialogTitle(t, "Recent Errors", inner, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)
	var parts []string
	if len(d.records) == 0 {
		parts = append(parts, "No errors have been reported in this session.")
	}
	for i := len(d.records) - 1; i >= 0; i-- {
		record := d.records[i]
		parts = append(parts, record.Time.Format(time.TimeOnly)+"\n"+record.Message)
	}
	plain := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, ansi.Strip(strings.Join(parts, "\n\n")))
	body := lipgloss.NewStyle().Width(inner).Padding(0, min(2, max(0, (inner-1)/2))).Render(plain)
	lines := strings.Split(body, "\n")
	help := ansi.Truncate("c/enter copy latest · a copy all · ↑/↓ scroll · esc close", inner, "")
	height := max(1, area.Dy()-frame.GetVerticalFrameSize()-lipgloss.Height(title)-lipgloss.Height(help)-2)
	d.maxScroll = max(0, len(lines)-height)
	d.scroll = max(0, min(d.scroll, d.maxScroll))
	visible := strings.Join(lines[d.scroll:min(len(lines), d.scroll+height)], "\n")
	view := frame.Render(strings.Join([]string{title, "", visible, "", help}, "\n"))
	DrawCenter(scr, area, view)
	return nil
}
