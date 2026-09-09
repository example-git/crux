package dialog

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/common"
)

type OAuthLoginProvider struct {
	Owner providerauth.Owner
	Name  string
}

// OAuthLoginPresentation is supplied by the main Update loop. The dialog
// presents the owner's status and emits actions without running authentication.
type OAuthLoginPresentation struct {
	Message, AuthorizationURL, UserCode string

	Editable, Retry, Open, Reload, Recover, RetryRecovery, CompleteDispatched bool
}

type ActionOAuthLoginSelect struct {
	Dialog *OAuthLogin
	Owner  providerauth.Owner
}
type ActionOAuthLoginSubmit struct{ Dialog *OAuthLogin }
type ActionOAuthLoginRetry struct{ Dialog *OAuthLogin }
type ActionOAuthLoginOpen struct{ Dialog *OAuthLogin }
type ActionOAuthLoginReload struct{ Dialog *OAuthLogin }
type ActionOAuthLoginRecover struct {
	Dialog *OAuthLogin
	Retry  bool
}

// OAuthLogin owns presentation and private input. The main model owns login
// requests, browser opening, callback listeners, retries and retained receipts.
type OAuthLogin struct {
	com          *common.Common
	isOnboarding bool
	providerName string
	providers    []OAuthLoginProvider
	selected     int
	revealChoice bool
	presentation OAuthLoginPresentation
	input        textinput.Model
	source       string
	details      viewport.Model
	help         help.Model

	submit, choose, previous, next, close, open, retry, reload, recover, retryRecovery, scroll key.Binding
}

func NewOAuthLogin(com *common.Common, isOnboarding bool, providerName string) *OAuthLogin {
	m := &OAuthLogin{com: com, isOnboarding: isOnboarding, providerName: providerName}
	m.input = textinput.New()
	m.input.CharLimit = 0 // The owner validates the original response's byte limit.
	m.input.SetVirtualCursor(false)
	m.input.SetStyles(com.Styles.TextInput)
	m.input.Placeholder = "Paste authorization response…"
	m.input.Prompt = "> "
	m.input.EchoMode = textinput.EchoPassword
	m.input.EchoCharacter = '•'
	m.details = viewport.New()
	m.help = help.New()
	m.help.Styles = com.Styles.DialogHelpStyles()
	m.submit = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "submit response"))
	m.choose = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select provider"))
	m.previous = key.NewBinding(key.WithKeys("up"), key.WithHelp("↑/↓", "providers"))
	m.next = key.NewBinding(key.WithKeys("down"))
	m.close = key.NewBinding(key.WithKeys("esc", "ctrl+c"), key.WithHelp("esc", "cancel"))
	m.open = key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("ctrl+o", "open authorization page"))
	m.retry = key.NewBinding(key.WithKeys("ctrl+t"), key.WithHelp("ctrl+t", "retry original receipt"))
	m.reload = key.NewBinding(key.WithKeys("ctrl+n"), key.WithHelp("ctrl+n", "reload sign-in status"))
	m.recover = key.NewBinding(key.WithKeys("alt+r"), key.WithHelp("alt+r", "attempt saved change recovery"))
	m.retryRecovery = key.NewBinding(key.WithKeys("alt+t"), key.WithHelp("alt+t", "retry recovery receipt"))
	m.scroll = key.NewBinding(key.WithKeys("pgup", "pgdown"), key.WithHelp("pgup/pgdn", "scroll"))
	m.SetPresentation(OAuthLoginPresentation{Message: "Loading sign-in status from the selected workspace…"})
	return m
}

func (m *OAuthLogin) ID() string { return LoginID }

func (m *OAuthLogin) SetProviders(providers []OAuthLoginProvider) {
	m.providers = slices.Clone(providers)
	m.selected = 0
	m.revealChoice = len(providers) > 0
	m.details.GotoTop()
	m.setBindings()
}

func (m *OAuthLogin) SetPresentation(p OAuthLoginPresentation) {
	if p.Message != m.presentation.Message || p.AuthorizationURL != m.presentation.AuthorizationURL || p.UserCode != m.presentation.UserCode {
		m.details.GotoTop()
	}
	m.presentation = p
	m.setBindings()
}

func (m *OAuthLogin) setBindings() {
	p := m.presentation
	m.choose.SetEnabled(len(m.providers) > 0)
	m.previous.SetEnabled(len(m.providers) > 0)
	m.next.SetEnabled(len(m.providers) > 0)
	m.submit.SetEnabled(p.Editable && len(m.providers) == 0)
	m.open.SetEnabled(p.Open)
	m.retry.SetEnabled(p.Retry)
	m.reload.SetEnabled(p.Reload)
	m.recover.SetEnabled(p.Recover)
	m.retryRecovery.SetEnabled(p.RetryRecovery)
	if p.CompleteDispatched {
		m.close.SetHelp("esc", "close; receipt retained")
	} else {
		m.close.SetHelp("esc", "cancel")
	}
	if m.submit.Enabled() {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
}

// TakeSource transfers the original bytes, including invalid or oversized
// input, so the main model can reject them visibly. No trimming or truncation
// is applied here. Later editing requires a new editable presentation.
func (m *OAuthLogin) TakeSource() string {
	source := m.source
	m.source = ""
	m.input.SetValue("")
	m.presentation.Editable = false
	m.setBindings()
	return source
}

func (m *OAuthLogin) HandleMsg(msg tea.Msg) Action {
	if press, ok := msg.(tea.KeyPressMsg); ok {
		switch {
		case key.Matches(press, m.close):
			return ActionClose{}
		case key.Matches(press, m.choose):
			return ActionOAuthLoginSelect{Dialog: m, Owner: m.providers[m.selected].Owner}
		case key.Matches(press, m.previous):
			m.selected = max(0, m.selected-1)
			m.revealChoice = true
			return nil
		case key.Matches(press, m.next):
			m.selected = min(len(m.providers)-1, m.selected+1)
			m.revealChoice = true
			return nil
		case key.Matches(press, m.open):
			return ActionOAuthLoginOpen{m}
		case key.Matches(press, m.retry):
			return ActionOAuthLoginRetry{m}
		case key.Matches(press, m.reload):
			return ActionOAuthLoginReload{m}
		case key.Matches(press, m.recover):
			return ActionOAuthLoginRecover{Dialog: m}
		case key.Matches(press, m.retryRecovery):
			return ActionOAuthLoginRecover{Dialog: m, Retry: true}
		case key.Matches(press, m.submit):
			return ActionOAuthLoginSubmit{m}
		case key.Matches(press, m.scroll):
			var cmd tea.Cmd
			m.details, cmd = m.details.Update(msg)
			if cmd != nil {
				return ActionCmd{cmd}
			}
			return nil
		}
	}
	if !m.submit.Enabled() {
		return nil
	}
	switch msg := msg.(type) {
	case tea.PasteMsg:
		m.insertSource(msg.Content)
	case tea.ClipboardMsg:
		m.insertSource(msg.String())
	case tea.KeyPressMsg:
		km := m.input.KeyMap
		if key.Matches(msg, km.Paste) {
			return ActionCmd{tea.ReadClipboard}
		}
		if !key.Matches(msg, km.CharacterForward, km.CharacterBackward, km.WordForward, km.WordBackward,
			km.DeleteWordBackward, km.DeleteWordForward, km.DeleteAfterCursor, km.DeleteBeforeCursor,
			km.DeleteCharacterBackward, km.DeleteCharacterForward, km.LineStart, km.LineEnd,
			km.AcceptSuggestion, km.NextSuggestion, km.PrevSuggestion) {
			m.insertSource(msg.Text)
			return nil
		}
		oldPosition, oldLength := m.input.Position(), utf8.RuneCountInString(m.input.Value())
		m.input, _ = m.input.Update(msg)
		if removed := oldLength - utf8.RuneCountInString(m.input.Value()); removed > 0 {
			start := min(oldPosition, m.input.Position())
			from, to := oauthSourceOffset(m.source, start), oauthSourceOffset(m.source, start+removed)
			m.source = m.source[:from] + m.source[to:]
		}
	}
	return nil
}

func (m *OAuthLogin) insertSource(value string) {
	if value == "" {
		return
	}
	position := m.input.Position()
	offset := oauthSourceOffset(m.source, position)
	m.source = m.source[:offset] + value + m.source[offset:]
	// Bubbles sanitizes controls and invalid UTF-8. A safe marker per source
	// rune preserves cursor/edit positions without putting any source in it.
	m.input.SetValue(strings.Repeat("•", utf8.RuneCountInString(m.source)))
	m.input.SetCursor(utf8.RuneCountInString(m.source[:offset+len(value)]))
}

func oauthSourceOffset(source string, position int) int {
	for offset := range source {
		if position == 0 {
			return offset
		}
		position--
	}
	return len(source)
}

func (m *OAuthLogin) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := m.com.Styles
	width := max(0, min(72, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	inner := max(0, width-t.Dialog.View.GetHorizontalFrameSize())
	frame := t.Dialog.View.GetVerticalFrameSize()
	if m.isOnboarding {
		frame = 0
	}
	available := max(0, area.Dy()-frame)
	m.input.SetWidth(dialogInputTextWidth(t, m.input, inner))
	name := "Sign in"
	if m.providerName != "" {
		name = m.providerName + " sign-in"
	}
	title := common.DialogTitle(t, name, inner, t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)
	input := ""
	if m.submit.Enabled() {
		input = t.Dialog.InputPrompt.Render(m.input.View())
	}
	footer := m.actionFooter(inner)
	var body []string
	selectedLine := 0
	for i, provider := range m.providers {
		style, marker := t.Dialog.PrimaryText, "  "
		if i == m.selected {
			selectedLine = lipgloss.Height(strings.Join(body, "\n"))
			if len(body) == 0 {
				selectedLine = 0
			}
			style, marker = t.Dialog.SelectedItem, "> "
		}
		body = append(body, style.Width(inner).Render(marker+provider.Name))
	}
	if m.presentation.CompleteDispatched {
		body = append(body, t.Dialog.SecondaryText.Width(inner).Render("Closing retains the original completion receipt."))
	}
	if m.presentation.Message != "" {
		body = append(body, t.Dialog.PrimaryText.Width(inner).Render(m.presentation.Message))
	}
	if m.presentation.AuthorizationURL != "" {
		body = append(body, t.Dialog.SecondaryText.Width(inner).Render("Authorization URL: "+m.presentation.AuthorizationURL))
	}
	if m.presentation.UserCode != "" {
		body = append(body, t.Dialog.PrimaryText.Width(inner).Render("Device code: "+m.presentation.UserCode))
	}
	m.details.SetWidth(inner)
	m.details.SetContent(strings.Join(body, "\n"))
	fixed := lipgloss.Height(title) + lipgloss.Height(footer)
	if input != "" {
		fixed += lipgloss.Height(input)
	}
	if fixed >= available {
		fixed -= lipgloss.Height(title)
		title = ""
	}
	if fixed >= available && input != "" {
		fixed -= lipgloss.Height(input)
		input = ""
	}
	m.details.SetHeight(min(max(0, available-fixed), m.details.TotalLineCount()))
	if m.revealChoice && m.details.Height() > 0 {
		if selectedLine < m.details.YOffset() {
			m.details.SetYOffset(selectedLine)
		} else if selectedLine >= m.details.YOffset()+m.details.Height() {
			m.details.SetYOffset(selectedLine - m.details.Height() + 1)
		}
		m.revealChoice = false
	}
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
	if input != "" {
		cur = InputCursor(t, m.input.Cursor())
		if cur != nil && title == "" {
			cur.Y -= t.Dialog.Title.GetVerticalFrameSize()
		}
	}
	if m.isOnboarding {
		DrawOnboardingCursor(scr, area, content, adjustOnboardingInputCursor(t, cur))
	} else {
		DrawCenterCursor(scr, area, t.Dialog.View.Width(width).Render(content), cur)
	}
	return cur
}

func (m *OAuthLogin) ShortHelp() []key.Binding {
	return []key.Binding{m.choose, m.previous, m.submit, m.open, m.retry, m.reload, m.recover, m.retryRecovery, m.close, m.scroll}
}
func (m *OAuthLogin) FullHelp() [][]key.Binding { return [][]key.Binding{m.ShortHelp()} }

func (m *OAuthLogin) actionFooter(width int) string {
	bindings := m.ShortHelp()
	descriptions := []string{"select", "providers", "submit", "open", "retry", "reload", "recover", "retry recovery", "cancel", "scroll"}
	if m.presentation.CompleteDispatched {
		descriptions[8] = "close"
	}
	textWidth := max(0, width-m.com.Styles.Dialog.HelpView.GetHorizontalFrameSize())
	var rows []string
	var row apiKeyHelpRow
	used := 0
	flush := func() {
		if len(row) > 0 {
			rows = append(rows, renderDialogHelp(m.com.Styles, &m.help, row, width))
			row, used = nil, 0
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

func (OAuthLogin) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth login dialog]"))
}
