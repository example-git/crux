package dialog

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/workspace"
)

const AuthenticationReconciliationID = "authentication_reconciliation"

// This component contains presentation only. The sole UI model dispatches all
// reviews and applies, retaining their identities independently of this dialog.
type AuthenticationReconciliation struct {
	com                                     *common.Common
	input                                   textinput.Model
	help                                    help.Model
	choice                                  workspace.ProviderAuthenticationReviewChoice
	accounts                                []AuthenticationRow
	accountIndex                            int
	generation                              uint64
	original, message, preview              string
	pending, retryReview, apply, retryApply bool
	finished                                bool
	scroll                                  int
}

type ActionAuthenticationReviewOpen struct{ Dialog *AccountAuthentication }
type ActionAuthenticationReconciliation struct {
	Dialog *AuthenticationReconciliation
	Kind   string // choice, review, retry-review, apply, retry-apply, cancel
	Choice workspace.ProviderAuthenticationReviewChoice
}

func NewAuthenticationReconciliation(com *common.Common, original string, accounts []AuthenticationRow) *AuthenticationReconciliation {
	d := &AuthenticationReconciliation{com: com, original: original, accounts: append([]AuthenticationRow(nil), accounts...), accountIndex: -1}
	d.input = textinput.New()
	d.input.SetVirtualCursor(false)
	d.input.SetStyles(com.Styles.TextInput)
	d.input.Placeholder = "Exact saved account ID"
	d.input.CharLimit = 256
	d.input.Focus()
	d.help = help.New()
	d.help.Styles = com.Styles.DialogHelpStyles()
	return d
}
func (d *AuthenticationReconciliation) ID() string         { return AuthenticationReconciliationID }
func (d *AuthenticationReconciliation) Generation() uint64 { return d.generation }
func (d *AuthenticationReconciliation) Choice() workspace.ProviderAuthenticationReviewChoice {
	return d.choice
}
func (d *AuthenticationReconciliation) SetChoice(choice workspace.ProviderAuthenticationReviewChoice) {
	d.choice = choice
	d.input.SetValue(choice.AccountID)
	d.generation++
	d.preview, d.apply, d.retryApply = "", false, false
}
func (d *AuthenticationReconciliation) SetState(message, preview string, pending, retryReview, apply, retryApply bool) {
	d.message, d.preview = message, preview
	d.pending, d.retryReview, d.apply, d.retryApply = pending, retryReview, apply, retryApply
}
func (d *AuthenticationReconciliation) SetFinished(finished bool) { d.finished = finished }
func (d *AuthenticationReconciliation) HandleMsg(msg tea.Msg) Action {
	if kp, ok := msg.(tea.KeyPressMsg); ok {
		switch kp.String() {
		case "esc", "alt+esc":
			return ActionClose{}
		case "ctrl+c":
			return ActionAuthenticationReconciliation{Dialog: d, Kind: "cancel"}
		case "pgdown":
			d.scroll += 5
			return nil
		case "pgup":
			d.scroll = max(0, d.scroll-5)
			return nil
		}
		if d.pending || d.finished {
			return nil
		}
		switch kp.String() {
		case "alt+1", "alt+2", "alt+3":
			choice := workspace.ProviderAuthenticationReviewChoice{}
			if kp.String() == "alt+2" {
				choice.Kind, choice.AccountID = "saved-account", d.input.Value()
				if choice.AccountID == "" && len(d.accounts) > 0 {
					d.accountIndex = 0
					choice.AccountID = d.accounts[0].AccountID
				}
			} else if kp.String() == "alt+3" {
				choice.Kind = "saved-logout"
			}
			d.SetChoice(choice)
			return ActionAuthenticationReconciliation{Dialog: d, Kind: "choice", Choice: choice}
		case "tab":
			if d.choice.Kind == "saved-account" && len(d.accounts) > 0 {
				d.accountIndex = (d.accountIndex + 1) % len(d.accounts)
				d.SetChoice(workspace.ProviderAuthenticationReviewChoice{Kind: "saved-account", AccountID: d.accounts[d.accountIndex].AccountID})
				return ActionAuthenticationReconciliation{Dialog: d, Kind: "choice", Choice: d.choice}
			}
			return nil
		case "enter":
			return ActionAuthenticationReconciliation{Dialog: d, Kind: "review", Choice: d.choice}
		case "ctrl+r":
			if d.retryReview {
				return ActionAuthenticationReconciliation{Dialog: d, Kind: "retry-review", Choice: d.choice}
			}
			return nil
		case "ctrl+y":
			if d.apply {
				return ActionAuthenticationReconciliation{Dialog: d, Kind: "apply", Choice: d.choice}
			}
			return nil
		case "ctrl+t":
			if d.retryApply {
				return ActionAuthenticationReconciliation{Dialog: d, Kind: "retry-apply", Choice: d.choice}
			}
			return nil
		}
	}
	if !d.pending && !d.finished && d.choice.Kind == "saved-account" {
		before := d.input.Value()
		d.input, _ = d.input.Update(msg)
		if before != d.input.Value() {
			d.choice.AccountID = d.input.Value()
			d.generation++
			d.preview, d.apply, d.retryApply = "", false, false
			return ActionAuthenticationReconciliation{Dialog: d, Kind: "choice", Choice: d.choice}
		}
	}
	return nil
}
func (d *AuthenticationReconciliation) ShortHelp() []key.Binding {
	bindings := []key.Binding{key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close"))}
	if d.finished {
		return bindings
	}
	if d.pending {
		return append(bindings, key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "cancel")))
	}
	bindings = append([]key.Binding{key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "new review"))}, bindings...)
	if d.apply {
		bindings = append([]key.Binding{key.NewBinding(key.WithKeys("ctrl+y"), key.WithHelp("ctrl+y", "apply preview"))}, bindings...)
	}
	if d.retryApply {
		bindings = append(bindings, key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", "retry apply")))
	}
	if d.retryReview {
		bindings = append(bindings, key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "retry review")))
	}
	return bindings
}
func (d *AuthenticationReconciliation) FullHelp() [][]key.Binding {
	return [][]key.Binding{d.ShortHelp()}
}
func (d *AuthenticationReconciliation) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(76, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	inner := max(0, width-t.Dialog.View.GetHorizontalFrameSize())
	choice := "Original request"
	if d.choice.Kind == "saved-account" {
		choice = "Saved account: " + d.choice.AccountID
	} else if d.choice.Kind == "saved-logout" {
		choice = "Saved logout"
	}
	text := "Publish reviewed saved state; the original operation is not repeated.\nAlt+1 original intent · Alt+2 saved account · Alt+3 saved logout\nChoice: " + choice
	if d.finished {
		text = "Reviewed publication completed.\nChoice: " + choice
	}
	if d.choice.Kind == "saved-account" {
		text += "\nType the exact account ID. Tab cycles known account rows."
		if d.accountIndex >= 0 && d.accountIndex < len(d.accounts) {
			text += "\nKnown account: " + d.accounts[d.accountIndex].Label
		}
	}
	text += "\n\n" + d.message
	if d.preview != "" {
		text += "\n\n" + d.preview
	}
	text += "\n\nOriginal result: " + d.original + "\nNo files are repaired or reloaded by review/apply."
	lines := strings.Split(ansi.Wrap(ansi.Strip(text), max(1, inner), ""), "\n")
	capacity := max(0, area.Dy()-t.Dialog.View.GetVerticalFrameSize()-5)
	d.scroll = min(d.scroll, max(0, len(lines)-capacity))
	end := min(len(lines), d.scroll+capacity)
	parts := []string{common.DialogTitle(t, "Review Saved Authentication", inner, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor), strings.Join(lines[d.scroll:end], "\n")}
	if len(lines) > capacity {
		parts = append(parts, t.Dialog.SecondaryText.Render(fmt.Sprintf("PgUp/PgDn scroll (%d–%d of %d)", d.scroll+1, end, len(lines))))
	}
	parts = append(parts, renderDialogHelp(t, &d.help, d, inner))
	view := t.Dialog.View.Width(width).Render(strings.Join(parts, "\n"))
	DrawCenterCursor(scr, area, view, nil)
	return nil
}
