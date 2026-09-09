package dialog

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/common"
)

const APIKeyInputID = "api_key_input"

type ActionAPIKeyCheck struct{ Dialog *APIKeyInput }
type ActionAPIKeySave struct{ Dialog *APIKeyInput }
type ActionAPIKeyRetry struct{ Dialog *APIKeyInput }
type ActionAPIKeyReload struct{ Dialog *APIKeyInput }
type ActionAPIKeyRecover struct {
	Dialog *APIKeyInput
	Retry  bool
}

// APIKeyPresentation is supplied only by the main Update loop. Source text is
// never copied into a status, completion or transport acknowledgement.
type APIKeyPresentation struct {
	SaveDispatched                                        bool
	Message, Evidence                                     string
	Editable, Save, Retry, Reload, Recover, RetryRecovery bool
}

// APIKeyInput owns editing and rendering. The workspace owner performs all
// status reads, expression resolution, probes and saves outside Update.
type APIKeyInput struct {
	details                                              viewport.Model
	scroll                                               key.Binding
	com                                                  *common.Common
	isOnboarding                                         bool
	providerName                                         string
	width                                                int
	input                                                textinput.Model
	help                                                 help.Model
	presentation                                         APIKeyPresentation
	submit, close, retry, reload, recover, retryRecovery key.Binding
}

func NewAPIKeyInput(com *common.Common, isOnboarding bool, selection ActionSelectModel) (*APIKeyInput, tea.Cmd) {
	m := &APIKeyInput{com: com, isOnboarding: isOnboarding, providerName: selection.Provider.Name}
	if m.providerName == "" {
		m.providerName = selection.Model.Provider
	}
	m.input = textinput.New()
	m.input.SetVirtualCursor(false)
	m.input.Placeholder = "Enter API key or expression…"
	m.input.SetStyles(com.Styles.TextInput)
	m.input.EchoMode = textinput.EchoPassword
	m.input.EchoCharacter = '•'
	m.details = viewport.New()
	m.scroll = key.NewBinding(key.WithKeys("pgup", "pgdown"), key.WithHelp("pgup/pgdn", "scroll"))
	m.help = help.New()
	m.help.Styles = com.Styles.DialogHelpStyles()
	m.submit = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "check input"))
	m.close = key.NewBinding(key.WithKeys("esc", "ctrl+c"), key.WithHelp("esc", "cancel"))
	m.retry = key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", "retry original receipt"))
	m.reload = key.NewBinding(key.WithKeys("ctrl+n"), key.WithHelp("ctrl+n", "new input / reload status"))
	m.recover = key.NewBinding(key.WithKeys("alt+r"), key.WithHelp("alt+r", "attempt saved change recovery"))
	m.retryRecovery = key.NewBinding(key.WithKeys("alt+t"), key.WithHelp("alt+t", "retry recovery receipt"))
	m.SetPresentation(APIKeyPresentation{Message: "Loading authentication status from the selected workspace…"})
	return m, nil
}
func (m *APIKeyInput) ID() string { return APIKeyInputID }
func (m *APIKeyInput) SetPresentation(p APIKeyPresentation) {
	if p.Message != m.presentation.Message || p.Evidence != m.presentation.Evidence {
		m.details.GotoTop()
	}
	m.presentation = p
	if p.SaveDispatched {
		m.close.SetHelp("esc", "close; receipt retained")
	} else {
		m.close.SetHelp("esc", "cancel")
	}
	m.retry.SetEnabled(p.Retry)
	m.reload.SetEnabled(p.Reload)
	m.recover.SetEnabled(p.Recover)
	m.retryRecovery.SetEnabled(p.RetryRecovery)
	m.submit.SetEnabled(p.Editable || p.Save)
	if p.Save {
		m.submit.SetHelp("enter", "save retained input and use model")
	} else {
		m.submit.SetHelp("enter", "check input")
	}
	if p.Editable {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
}

// TakeSource transfers input into the private request retained by the main
// model. Editing and paste remain disabled after this transfer.
func (m *APIKeyInput) TakeSource() string {
	value := m.input.Value()
	m.input.SetValue("")
	m.presentation.Editable = false
	m.input.Blur()
	return value
}
func (m *APIKeyInput) HandleMsg(msg tea.Msg) Action {
	if press, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case key.Matches(press, m.scroll):
			var cmd tea.Cmd
			m.details, cmd = m.details.Update(msg)
			if cmd != nil {
				return ActionCmd{cmd}
			}
			return nil
		case key.Matches(press, m.close):
			return ActionClose{}
		case key.Matches(press, m.retry):
			return ActionAPIKeyRetry{m}
		case key.Matches(press, m.reload):
			return ActionAPIKeyReload{m}
		case key.Matches(press, m.recover):
			return ActionAPIKeyRecover{Dialog: m}
		case key.Matches(press, m.retryRecovery):
			return ActionAPIKeyRecover{Dialog: m, Retry: true}
		case key.Matches(press, m.submit):
			if m.presentation.Save {
				return ActionAPIKeySave{m}
			}
			if m.presentation.Editable && m.input.Value() != "" {
				return ActionAPIKeyCheck{m}
			}
		}
	}
	if !m.presentation.Editable {
		return nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if cmd != nil {
		return ActionCmd{cmd}
	}
	return nil
}
func (m *APIKeyInput) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := m.com.Styles
	m.width = max(0, min(72, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	inner := max(0, m.width-t.Dialog.View.GetHorizontalFrameSize())
	frame := t.Dialog.View.GetVerticalFrameSize()
	if m.isOnboarding {
		frame = 0
	}
	available := max(0, area.Dy()-frame)
	m.input.SetWidth(max(0, inner-t.Dialog.InputPrompt.GetHorizontalFrameSize()-1))
	m.input.Prompt = "> "
	title := common.DialogTitle(t, m.providerName+" API key", inner, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)
	input := ""
	if m.presentation.Editable {
		input = t.Dialog.InputPrompt.Render(m.input.View())
	}
	footer := m.actionFooter(inner)
	body := t.Dialog.PrimaryText.Width(inner).Render(m.presentation.Message)
	if m.presentation.Evidence != "" {
		body += "\n" + t.Dialog.SecondaryText.Width(inner).Render(m.presentation.Evidence)
	}
	body += "\n" + t.Dialog.SecondaryText.Width(inner).Render("Save destination: the selected workspace credential owner's global configuration.")
	m.details.SetWidth(inner)
	m.details.SetContent(body)
	fixed := lipgloss.Height(title) + lipgloss.Height(footer)
	if input != "" {
		fixed += lipgloss.Height(input)
	}
	if fixed >= available && input != "" {
		fixed -= lipgloss.Height(input)
		input = ""
	}
	if fixed >= available {
		fixed -= lipgloss.Height(title)
		title = ""
	}
	m.details.SetHeight(min(max(0, available-fixed), m.details.TotalLineCount()))
	var parts []string
	if title != "" {
		parts = append(parts, title)
	}
	if input != "" {
		parts = append(parts, input)
	}
	if m.details.Height() > 0 {
		parts = append(parts, m.details.View())
	}
	parts = append(parts, footer)
	content := strings.Join(parts, "\n")
	var cur *tea.Cursor
	if m.presentation.Editable && input != "" {
		cur = InputCursor(t, m.input.Cursor())
	}
	if m.isOnboarding {
		DrawOnboardingCursor(scr, area, content, adjustOnboardingInputCursor(t, cur))
	} else {
		DrawCenterCursor(scr, area, t.Dialog.View.Width(m.width).Render(content), cur)
	}
	return cur
}

type apiKeyHelpRow []key.Binding

func (r apiKeyHelpRow) ShortHelp() []key.Binding  { return r }
func (r apiKeyHelpRow) FullHelp() [][]key.Binding { return [][]key.Binding{r} }

// Keep every enabled action in the fixed footer. Compact descriptions fit
// small terminals; the full explanation stays in the scrollable details pane.
func (m *APIKeyInput) actionFooter(width int) string {
	bindings := []key.Binding{m.submit, m.close, m.retry, m.recover, m.retryRecovery, m.reload, m.scroll}
	descriptions := []string{"check", "cancel", "retry", "recover", "retry recovery", "new input", "scroll"}
	if m.presentation.Save {
		descriptions[0] = "save"
	}
	if m.presentation.SaveDispatched {
		descriptions[1] = "close"
	}
	textWidth := max(0, width-m.com.Styles.Dialog.HelpView.GetHorizontalFrameSize())
	var rows []string
	var row apiKeyHelpRow
	used := 0
	flush := func() {
		if len(row) > 0 {
			rows = append(rows, renderDialogHelp(m.com.Styles, &m.help, row, width))
			row = nil
			used = 0
		}
	}
	for i, binding := range bindings {
		if !binding.Enabled() {
			continue
		}
		binding.SetHelp(binding.Help().Key, descriptions[i])
		size := ansi.StringWidth(binding.Help().Key + " " + binding.Help().Desc)
		if len(row) > 0 && used+ansi.StringWidth(m.help.ShortSeparator)+size > textWidth {
			flush()
		}
		if len(row) > 0 {
			used += ansi.StringWidth(m.help.ShortSeparator)
		}
		row = append(row, binding)
		used += size
	}
	flush()
	return strings.Join(rows, "\n")
}

func (m *APIKeyInput) ShortHelp() []key.Binding {
	return []key.Binding{m.submit, m.retry, m.reload, m.recover, m.retryRecovery, m.close}
}
func (m *APIKeyInput) FullHelp() [][]key.Binding { return [][]key.Binding{m.ShortHelp()} }

// APIKeyProbeDescription reports observations without claiming inference or
// selected-model authorization. HTTP evidence remains useful on failed checks.
func APIKeyProbeDescription(p config.ConnectionProbeResult) string {
	var text string
	switch p.Kind {
	case config.ConnectionProbeNotProbed:
		text = "No network probe was performed."
	case config.ConnectionProbeFormatOnly:
		text = "Format check only (sk- prefix); no network probe was performed."
	case config.ConnectionProbeHTTPAttempt:
		text = "HTTP request attempted; no response was observed."
	case config.ConnectionProbeHTTPResponse:
		text = fmt.Sprintf("HTTP %d observed", p.HTTPStatus)
		if p.Policy == config.ConnectionProbePolicyNon401 {
			text += " under the provider's non-401 policy."
		} else {
			text += " from the models probe (HTTP 200 policy)."
		}
	case config.ConnectionProbeUnsupported:
		text = "This provider's connection probe is unsupported."
	default:
		return ""
	}
	if p.AuthorizationOverridden {
		text += " The configured Authorization header replaced the entered key."
	} else if p.EnteredKeyInAuthorization {
		text += " The initial request used the entered key in Authorization."
	}
	return text + " This does not establish permission to run the selected model."
}
