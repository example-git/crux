package dialog

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/foundation/bubbles/help"
	"github.com/example-git/crux/foundation/bubbles/key"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/workspace"
)

const SavedAuthenticationID = "saved_authentication"

type SavedAuthenticationChoice struct {
	Target providerauth.Target
	Choice workspace.ProviderAuthenticationReviewChoice
	Label  string
	Slots  []providerauth.CredentialSlot
}
type ActionSavedAuthentication struct {
	Dialog    *SavedAuthentication
	Kind      string
	Selection SavedAuthenticationChoice
}

// SavedAuthentication contains presentation only; reads, reload and review
// dispatch belong to the main UI model.
type SavedAuthentication struct {
	com                  *common.Common
	help                 help.Model
	rows                 []SavedAuthenticationChoice
	selected             int
	noticeScroll         int
	message              string
	pending, retryReload bool
}

func NewSavedAuthentication(com *common.Common) *SavedAuthentication {
	d := &SavedAuthentication{com: com, message: "Reading current local saved authentication…", pending: true}
	d.help = help.New()
	d.help.Styles = com.Styles.DialogHelpStyles()
	return d
}
func (d *SavedAuthentication) ID() string { return SavedAuthenticationID }
func (d *SavedAuthentication) SetRows(rows []SavedAuthenticationChoice) {
	d.rows = rows
	d.selected = 0
}
func (d *SavedAuthentication) SetState(message string, pending, retry bool) {
	d.message, d.pending, d.retryReload = message, pending, retry
}
func (d *SavedAuthentication) HandleMsg(msg tea.Msg) Action {
	kp, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	if kp.String() == "esc" {
		return ActionClose{}
	}
	if kp.String() == "pgdown" {
		d.noticeScroll++
		return nil
	}
	if kp.String() == "pgup" {
		d.noticeScroll = max(0, d.noticeScroll-1)
		return nil
	}
	if kp.String() == "ctrl+c" {
		return ActionSavedAuthentication{Dialog: d, Kind: "cancel"}
	}
	if d.pending {
		return nil
	}
	switch kp.String() {
	case "up", "k":
		d.selected = max(0, d.selected-1)
	case "down", "j":
		d.selected = min(max(0, len(d.rows)-1), d.selected+1)
	case "ctrl+r":
		return ActionSavedAuthentication{Dialog: d, Kind: "status"}
	case "ctrl+t":
		if d.retryReload {
			return ActionSavedAuthentication{Dialog: d, Kind: "retry-reload"}
		}
	case "enter", "ctrl+l":
		if len(d.rows) > 0 {
			kind := "review"
			if kp.String() == "ctrl+l" {
				kind = "reload"
			}
			return ActionSavedAuthentication{Dialog: d, Kind: kind, Selection: d.rows[d.selected]}
		}
	}
	return nil
}
func (d *SavedAuthentication) ShortHelp() []key.Binding {
	result := []key.Binding{key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close"))}
	if d.pending {
		return append(result, key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "cancel")))
	}
	result = append([]key.Binding{key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "review choice")), key.NewBinding(key.WithKeys("ctrl+l"), key.WithHelp("ctrl+l", "reload files")), key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "read status"))}, result...)
	if d.retryReload {
		result = append(result, key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", "retry reload receipt")))
	}
	return result
}
func (d *SavedAuthentication) FullHelp() [][]key.Binding { return [][]key.Binding{d.ShortHelp()} }
func (d *SavedAuthentication) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(76, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	inner := max(0, width-t.Dialog.View.GetHorizontalFrameSize())
	footer := savedAuthenticationFooter(d.com, &d.help, d.ShortHelp(), inner)
	lines := []string{common.DialogTitle(t, "Saved Authentication", inner, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)}
	notice := ansi.Wrap(ansi.Strip(d.message), max(1, inner), "")
	available := max(0, area.Dy()-t.Dialog.View.GetVerticalFrameSize()-len(strings.Split(footer, "\n"))-3)
	noticeLines := strings.Split(notice, "\n")
	noticeRows := min(2, max(0, available-1), len(noticeLines))
	d.noticeScroll = min(d.noticeScroll, max(0, len(noticeLines)-noticeRows))
	lines = append(lines, strings.Join(noticeLines[d.noticeScroll:d.noticeScroll+noticeRows], "\n"))
	available -= noticeRows
	start := max(0, d.selected-max(0, available-1))
	end := min(len(d.rows), start+available)
	for i := start; i < end; i++ {
		prefix := "  "
		if i == d.selected {
			prefix = "> "
		}
		lines = append(lines, ansi.Truncate(prefix+ansi.Strip(d.rows[i].Label), max(0, inner), "…"))
	}
	if len(d.rows) > available {
		lines = append(lines, ansi.Truncate(fmt.Sprintf("↑/↓ %d–%d/%d · PgUp/Dn message", start+1, end, len(d.rows)), max(0, inner), "…"))
	}
	lines = append(lines, footer)
	DrawCenterCursor(scr, area, t.Dialog.View.Width(width).Render(strings.Join(lines, "\n")), nil)
	return nil
}

// Keep every action visible instead of truncating a one-line help footer.
func savedAuthenticationFooter(com *common.Common, h *help.Model, bindings []key.Binding, width int) string {
	textWidth := max(0, width-com.Styles.Dialog.HelpView.GetHorizontalFrameSize())
	var rows []string
	var row apiKeyHelpRow
	used := 0
	flush := func() {
		if len(row) > 0 {
			rows = append(rows, renderDialogHelp(com.Styles, h, row, width))
			row = nil
			used = 0
		}
	}
	for _, binding := range bindings {
		if !binding.Enabled() {
			continue
		}
		size := ansi.StringWidth(binding.Help().Key + " " + binding.Help().Desc)
		separator := ansi.StringWidth(h.ShortSeparator)
		if len(row) > 0 && used+separator+size > textWidth {
			flush()
		}
		if len(row) > 0 {
			used += separator
		}
		row = append(row, binding)
		used += size
	}
	flush()
	return strings.Join(rows, "\n")
}
