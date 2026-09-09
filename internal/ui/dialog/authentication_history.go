package dialog

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/ui/common"
)

const AuthenticationHistoryID = "authentication_history"

type AuthenticationHistoryRow struct {
	Key, Label, Details                                 string
	Review, Recover, RetryRecovery, Repair, ApplyRepair bool
}
type ActionAuthenticationHistory struct {
	Dialog     *AuthenticationHistory
	Kind, Key  string
	Generation uint64
}

// AuthenticationHistory contains display rows only. All operation identities
// and workspace effects stay with the sole top-level UI model.
type AuthenticationHistory struct {
	com              *common.Common
	help             help.Model
	rows             []AuthenticationHistoryRow
	selected, scroll int
	generation       uint64
	detail, pending  bool
	message          string
}

func NewAuthenticationHistory(com *common.Common) *AuthenticationHistory {
	d := &AuthenticationHistory{com: com, pending: true, message: "Reading retained authentication history…"}
	d.help = help.New()
	d.help.Styles = com.Styles.DialogHelpStyles()
	return d
}
func (d *AuthenticationHistory) ID() string         { return AuthenticationHistoryID }
func (d *AuthenticationHistory) Generation() uint64 { return d.generation }
func (d *AuthenticationHistory) SelectedKey() string {
	if len(d.rows) == 0 {
		return ""
	}
	return d.rows[d.selected].Key
}
func (d *AuthenticationHistory) SetRows(rows []AuthenticationHistoryRow) {
	selected := d.SelectedKey()
	d.rows = append([]AuthenticationHistoryRow(nil), rows...)
	d.selected = 0
	for i, row := range d.rows {
		if row.Key == selected {
			d.selected = i
			break
		}
	}
	if d.SelectedKey() != selected {
		d.generation++
		d.scroll = 0
	}
}
func (d *AuthenticationHistory) SetState(message string, pending bool) {
	d.message, d.pending = message, pending
}
func (d *AuthenticationHistory) ShowDetails() { d.detail = true; d.scroll = 0; d.generation++ }
func (d *AuthenticationHistory) HandleMsg(msg tea.Msg) Action {
	kp, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	name := kp.String()
	action := func(kind string) Action {
		return ActionAuthenticationHistory{Dialog: d, Kind: kind, Key: d.SelectedKey(), Generation: d.generation}
	}
	switch name {
	case "esc":
		if d.detail {
			d.detail = false
			d.scroll = 0
			d.generation++
			return nil
		}
		return ActionClose{}
	case "ctrl+c":
		return action("cancel")
	case "pgdown":
		d.scroll += 3
		return nil
	case "pgup":
		d.scroll = max(0, d.scroll-3)
		return nil
	}
	if d.pending {
		return nil
	}
	switch name {
	case "ctrl+r":
		return action("refresh")
	case "ctrl+l":
		return action("saved")
	}
	if len(d.rows) == 0 {
		return nil
	}
	row := d.rows[d.selected]
	if !d.detail {
		switch name {
		case "up", "k":
			d.selected = max(0, d.selected-1)
			d.generation++
		case "down", "j":
			d.selected = min(len(d.rows)-1, d.selected+1)
			d.generation++
		case "enter":
			return action("open")
		}
		return nil
	}
	switch name {
	case "enter":
		if row.Review {
			return action("review")
		}
	case "alt+r":
		if row.Recover {
			return action("recover")
		}
	case "alt+t":
		if row.RetryRecovery {
			return action("retry-recovery")
		}
	case "ctrl+p":
		if row.Repair {
			return action("repair")
		}
	case "ctrl+y":
		if row.ApplyRepair {
			return action("apply-repair")
		}
	}
	return nil
}
func (d *AuthenticationHistory) ShortHelp() []key.Binding {
	add := func(k, label string) key.Binding { return key.NewBinding(key.WithKeys(k), key.WithHelp(k, label)) }
	result := []key.Binding{}
	if d.pending {
		return []key.Binding{add("ctrl+c", "cancel wait"), add("esc", "close")}
	}
	if !d.detail {
		result = append(result, add("enter", "open"), add("↑/↓", "select"))
	} else if len(d.rows) > 0 {
		row := d.rows[d.selected]
		if row.Review {
			result = append(result, add("enter", "review"))
		}
		if row.Recover {
			result = append(result, add("alt+r", "recover"))
		}
		if row.RetryRecovery {
			result = append(result, add("alt+t", "retry"))
		}
		if row.Repair {
			result = append(result, add("ctrl+p", "repair review"))
		}
		if row.ApplyRepair {
			result = append(result, add("ctrl+y", "apply repair"))
		}
	}
	return append(result, add("ctrl+r", "history"), add("ctrl+l", "saved"), add("esc", "back"))
}
func (d *AuthenticationHistory) FullHelp() [][]key.Binding { return [][]key.Binding{d.ShortHelp()} }
func (d *AuthenticationHistory) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(78, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	inner := max(0, width-t.Dialog.View.GetHorizontalFrameSize())
	footer := savedAuthenticationFooter(d.com, &d.help, d.ShortHelp(), inner)
	available := max(0, area.Dy()-t.Dialog.View.GetVerticalFrameSize()-len(strings.Split(footer, "\n"))-2)
	lines := []string{common.DialogTitle(t, "Authentication History", inner, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)}
	if d.detail {
		text := d.message
		if len(d.rows) > 0 {
			text = d.rows[d.selected].Label + "\n" + d.rows[d.selected].Details + "\n\n" + text
		}
		body := strings.Split(ansi.Wrap(ansi.Strip(text), max(1, inner), ""), "\n")
		d.scroll = min(d.scroll, max(0, len(body)-available))
		end := min(len(body), d.scroll+available)
		lines = append(lines, strings.Join(body[d.scroll:end], "\n"))
		if len(body) > available {
			lines = append(lines, ansi.Truncate(fmt.Sprintf("PgUp/PgDn %d–%d/%d", d.scroll+1, end, len(body)), inner, "…"))
		}
	} else {
		if available > 0 {
			lines = append(lines, ansi.Truncate(ansi.Strip(d.message), inner, "…"))
			available--
		}
		start := max(0, d.selected-max(0, available-1))
		end := min(len(d.rows), start+available)
		for i := start; i < end; i++ {
			prefix := "  "
			if i == d.selected {
				prefix = "> "
			}
			lines = append(lines, ansi.Truncate(prefix+ansi.Strip(d.rows[i].Label), inner, "…"))
		}
		if len(d.rows) > available {
			lines = append(lines, ansi.Truncate(fmt.Sprintf("%d–%d/%d retained entries", start+1, end, len(d.rows)), inner, "…"))
		}
	}
	lines = append(lines, footer)
	DrawCenterCursor(scr, area, t.Dialog.View.Width(width).Render(strings.Join(lines, "\n")), nil)
	return nil
}
