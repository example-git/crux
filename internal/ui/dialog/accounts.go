package dialog

// Account management dialogs: /login, /logout, and the account switcher.
// All three are summoned from the commands (settings) menu rather than key
// combos. The switcher lists every stored OAuth account across providers and
// activates the selected one, pushing its credential into the provider
// config so the next request uses it.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/list"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/pkg/browser"
	"github.com/sahilm/fuzzy"
)

// Dialog identifiers.
const (
	LoginID           = "login"
	LogoutID          = "logout"
	AccountSwitcherID = "account_switcher"

	accountsDialogMaxWidth  = 60
	accountsDialogMaxHeight = 16
)

// oauthProviderChoice is one selectable provider in the login/logout lists.
type oauthProviderChoice struct {
	cruxID string
	label  string
}

func oauthProviderChoices(cfg *config.Config) []oauthProviderChoice {
	var choices []oauthProviderChoice
	for _, registration := range cfg.ProviderRegistrations() {
		if registration.OAuth != nil {
			choices = append(choices, oauthProviderChoice{cruxID: registration.ProviderID, label: registration.Name})
		}
	}
	return choices
}

// ---------------------------------------------------------------------------
// Shared picker item
// ---------------------------------------------------------------------------

// accountPickItem is a generic list row used by all three dialogs.
type accountPickItem struct {
	*list.Versioned
	id      string
	label   string
	info    string
	t       *styles.Styles
	m       fuzzy.Match
	cache   map[int]string
	focused bool
}

var _ ListItem = (*accountPickItem)(nil)

func newAccountPickItem(t *styles.Styles, id, label, info string) *accountPickItem {
	return &accountPickItem{Versioned: list.NewVersioned(), id: id, label: label, info: info, t: t}
}

func (a *accountPickItem) Finished() bool { return true }
func (a *accountPickItem) Filter() string { return a.label }
func (a *accountPickItem) ID() string     { return a.id }

func (a *accountPickItem) SetFocused(focused bool) {
	if a.focused == focused {
		return
	}
	a.cache = nil
	a.focused = focused
	if a.Versioned != nil {
		a.Bump()
	}
}

func (a *accountPickItem) SetMatch(m fuzzy.Match) {
	if sameFuzzyMatch(a.m, m) {
		return
	}
	a.cache = nil
	a.m = m
	if a.Versioned != nil {
		a.Bump()
	}
}

func (a *accountPickItem) Render(width int) string {
	st := ListItemStyles{
		ItemBlurred:     a.t.Dialog.NormalItem,
		ItemFocused:     a.t.Dialog.SelectedItem,
		InfoTextBlurred: a.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: a.t.Dialog.ListItem.InfoFocused,
	}
	return renderItem(st, a.label, a.info, a.focused, width, a.cache, &a.m)
}

// ---------------------------------------------------------------------------
// Base picker dialog
// ---------------------------------------------------------------------------

// accountsPicker is the shared list+filter scaffolding.
type accountsPicker struct {
	com   *common.Common
	help  help.Model
	list  *list.FilterableList
	input textinput.Model

	keyMap struct {
		Select   key.Binding
		Next     key.Binding
		Previous key.Binding
		UpDown   key.Binding
		Close    key.Binding
	}
}

func newAccountsPicker(com *common.Common) accountsPicker {
	p := accountsPicker{com: com}
	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	p.help = h

	p.list = list.NewFilterableList()
	p.list.Focus()

	p.input = textinput.New()
	p.input.SetVirtualCursor(false)
	p.input.Placeholder = "Type to filter"
	p.input.SetStyles(com.Styles.TextInput)
	p.input.Focus()

	p.keyMap.Select = key.NewBinding(key.WithKeys("enter", "ctrl+y"), key.WithHelp("enter", "confirm"))
	p.keyMap.Next = key.NewBinding(key.WithKeys("down", "ctrl+n"), key.WithHelp("↓", "next item"))
	p.keyMap.Previous = key.NewBinding(key.WithKeys("up", "ctrl+p"), key.WithHelp("↑", "previous item"))
	p.keyMap.UpDown = key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑/↓", "choose"))
	p.keyMap.Close = CloseKey
	return p
}

// handleNav processes navigation/filter keys; returns (action, handled).
func (p *accountsPicker) handleNav(msg tea.KeyPressMsg) (Action, bool) {
	switch {
	case key.Matches(msg, p.keyMap.Close):
		return ActionClose{}, true
	case key.Matches(msg, p.keyMap.Previous):
		p.list.Focus()
		if p.list.IsSelectedFirst() {
			p.list.SelectLast()
			p.list.ScrollToBottom()
			return nil, true
		}
		p.list.SelectPrev()
		p.list.ScrollToSelected()
		return nil, true
	case key.Matches(msg, p.keyMap.Next):
		p.list.Focus()
		if p.list.IsSelectedLast() {
			p.list.SelectFirst()
			p.list.ScrollToTop()
			return nil, true
		}
		p.list.SelectNext()
		p.list.ScrollToSelected()
		return nil, true
	}
	return nil, false
}

func (p *accountsPicker) handleFilter(msg tea.KeyPressMsg) Action {
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	p.list.SetFilter(p.input.Value())
	p.list.ScrollToTop()
	p.list.SetSelected(0)
	return ActionCmd{cmd}
}

func (p *accountsPicker) selected() *accountPickItem {
	item := p.list.SelectedItem()
	if item == nil {
		return nil
	}
	pick, _ := item.(*accountPickItem)
	return pick
}

func (p *accountsPicker) draw(scr uv.Screen, area uv.Rectangle, title string, keyMap help.KeyMap) *tea.Cursor {
	t := p.com.Styles
	width := max(0, min(accountsDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(accountsDialogMaxHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.InputPrompt.GetVerticalFrameSize() + inputContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() +
		t.Dialog.View.GetVerticalFrameSize()

	p.input.SetWidth(dialogInputTextWidth(t, p.input, innerWidth))
	p.list.SetSize(innerWidth, max(0, height-heightOffset))

	rc := NewRenderContext(t, width)
	rc.Title = title
	rc.AddPart(t.Dialog.InputPrompt.Render(p.input.View()))

	if p.list.Height() >= len(p.list.FilteredItems()) {
		p.list.ScrollToTop()
	} else {
		p.list.ScrollToSelected()
	}
	rc.AddPart(t.Dialog.List.Height(p.list.Height()).Render(p.list.Render()))
	rc.Help = renderDialogHelp(t, &p.help, keyMap, innerWidth)

	view := rc.Render()
	cur := InputCursor(p.com.Styles, p.input.Cursor())
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

func (p *accountsPicker) shortHelp() []key.Binding {
	return []key.Binding{p.keyMap.UpDown, p.keyMap.Select, p.keyMap.Close}
}

func (p *accountsPicker) fullHelp() [][]key.Binding {
	return [][]key.Binding{{p.keyMap.Select, p.keyMap.Next, p.keyMap.Previous, p.keyMap.Close}}
}

// ---------------------------------------------------------------------------
// Account switcher
// ---------------------------------------------------------------------------

// AccountSwitcher lists every stored OAuth account across providers and
// activates the selected one.
type AccountSwitcher struct {
	accountsPicker
}

var _ Dialog = (*AccountSwitcher)(nil)

// NewAccountSwitcher creates the account switcher dialog.
func NewAccountSwitcher(com *common.Common) *AccountSwitcher {
	s := &AccountSwitcher{accountsPicker: newAccountsPicker(com)}
	s.setItems()
	return s
}

// ID implements Dialog.
func (s *AccountSwitcher) ID() string { return AccountSwitcherID }

func (s *AccountSwitcher) setItems() {
	ctx := context.Background()
	var items []list.FilterableItem
	cfg := s.com.Config()
	providers, err := accounts.ProvidersFor(ctx, cfg.ProviderAccountNamespaces())
	if err != nil {
		return
	}
	providers = activeAccountProviders(providers, func(provider string) bool {
		_, ok := cfg.ProviderRegistrationForAccount(provider)
		return ok
	})
	for _, provider := range providers {
		entries, err := accounts.List(ctx, provider)
		if err != nil {
			continue
		}
		active, _ := accounts.Active(ctx, provider)
		for _, entry := range entries {
			info := provider
			if active != nil && entry.ID == active.ID {
				info = provider + " · active"
			}
			items = append(items, newAccountPickItem(
				s.com.Styles,
				provider+"\x00"+entry.ID,
				entry.DisplayName,
				info,
			))
		}
	}
	s.list.SetItems(items...)
	s.list.SetSelected(0)
}

func activeAccountProviders(stored []string, active func(string) bool) []string {
	result := make([]string, 0, len(stored))
	for _, provider := range stored {
		if active(provider) {
			result = append(result, provider)
		}
	}
	return result
}

// HandleMsg implements Dialog.
func (s *AccountSwitcher) HandleMsg(msg tea.Msg) Action {
	if kp, ok := msg.(tea.KeyPressMsg); ok {
		if action, handled := s.handleNav(kp); handled {
			return action
		}
		if key.Matches(kp, s.keyMap.Select) {
			pick := s.selected()
			if pick == nil {
				return nil
			}
			provider, id, ok := strings.Cut(pick.id, "\x00")
			if !ok {
				return nil
			}
			return ActionSwitchAccount{Provider: provider, AccountID: id, DisplayName: pick.label}
		}
		return s.handleFilter(kp)
	}
	return nil
}

// Cursor implements Dialog.
func (s *AccountSwitcher) Cursor() *tea.Cursor {
	return InputCursor(s.com.Styles, s.input.Cursor())
}

// Draw implements Dialog.
func (s *AccountSwitcher) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	return s.draw(scr, area, "Switch Account", s)
}

// ShortHelp implements help.KeyMap.
func (s *AccountSwitcher) ShortHelp() []key.Binding { return s.shortHelp() }

// FullHelp implements help.KeyMap.
func (s *AccountSwitcher) FullHelp() [][]key.Binding { return s.fullHelp() }

// ActionSwitchAccount is emitted when the user picks an account.
type ActionSwitchAccount struct {
	Provider    string // accounts store key
	AccountID   string
	DisplayName string
}

// SwitchAccount activates the account and pushes its (fresh) credential into
// the provider config. Returned as a tea.Cmd result message.
type AccountSwitchedMsg struct {
	CruxProviderID string
	DisplayName    string
	Continuation   *ActionSelectModel
	Err            error
}

// SwitchAccountCmd performs the switch in the background.
func SwitchAccountCmd(com *common.Common, action ActionSwitchAccount) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		registration, ok := com.Config().ProviderRegistrationForAccount(action.Provider)
		if !ok {
			return AccountSwitchedMsg{DisplayName: action.DisplayName, Err: fmt.Errorf("provider account owner %s is not active", action.Provider)}
		}
		cruxID := registration.ProviderID
		accountNamespace := registration.AccountNamespace
		owner := registration.Owner()

		entries, err := accounts.List(ctx, accountNamespace)
		if err != nil {
			return AccountSwitchedMsg{CruxProviderID: cruxID, DisplayName: action.DisplayName, Err: err}
		}
		var entry *accounts.Entry
		for index := range entries {
			if entries[index].ID == action.AccountID {
				entry = &entries[index]
				break
			}
		}
		if entry == nil {
			return AccountSwitchedMsg{CruxProviderID: cruxID, DisplayName: action.DisplayName, Err: fmt.Errorf("account %q not found for provider %q", action.AccountID, accountNamespace)}
		}
		validate := func() error {
			current, ok := com.Config().ProviderOwner(cruxID)
			if !ok || current != owner {
				return fmt.Errorf("provider account owner changed for %s", action.Provider)
			}
			return nil
		}
		if err := validate(); err != nil {
			return AccountSwitchedMsg{CruxProviderID: cruxID, DisplayName: action.DisplayName, Err: err}
		}
		var refresher accounts.Refresher
		if registration.OAuth != nil {
			refresher = registration.OAuth.Refresh
		}
		fresh, err := accounts.EnsureFreshForOwner(ctx, accountNamespace, entry, refresher, validate)
		if err != nil {
			return AccountSwitchedMsg{CruxProviderID: cruxID, DisplayName: action.DisplayName, Err: err}
		}
		if err := validate(); err != nil {
			return AccountSwitchedMsg{CruxProviderID: cruxID, DisplayName: action.DisplayName, Err: err}
		}
		credential := config.ProviderOAuthCredential{Owner: owner, Token: fresh.Token()}
		if err := com.Workspace.SetProviderAPIKey(config.ScopeGlobal, cruxID, credential); err != nil {
			return AccountSwitchedMsg{CruxProviderID: cruxID, DisplayName: action.DisplayName, Err: err}
		}
		if err := validate(); err != nil {
			return AccountSwitchedMsg{CruxProviderID: cruxID, DisplayName: action.DisplayName, Err: err}
		}
		if err := accounts.SetActiveForOwner(ctx, accountNamespace, action.AccountID, validate); err != nil {
			return AccountSwitchedMsg{CruxProviderID: cruxID, DisplayName: action.DisplayName, Err: err}
		}
		return AccountSwitchedMsg{CruxProviderID: cruxID, DisplayName: action.DisplayName}
	}
}

// ---------------------------------------------------------------------------
// Logout dialog
// ---------------------------------------------------------------------------

// Logout lists logged-in OAuth providers; selecting one removes its
// credentials.
type Logout struct {
	accountsPicker
}

var _ Dialog = (*Logout)(nil)

// NewLogout creates the logout dialog.
func NewLogout(com *common.Common) *Logout {
	l := &Logout{accountsPicker: newAccountsPicker(com)}
	l.setItems()
	return l
}

// ID implements Dialog.
func (l *Logout) ID() string { return LogoutID }

func (l *Logout) setItems() {
	cfg := l.com.Config()
	var items []list.FilterableItem
	for _, choice := range oauthProviderChoices(cfg) {
		if cfg == nil {
			continue
		}
		if pc, ok := cfg.Providers.Get(choice.cruxID); ok && (pc.OAuthToken != nil || pc.APIKey != "") {
			items = append(items, newAccountPickItem(l.com.Styles, choice.cruxID, choice.label, "logged in"))
		}
	}
	l.list.SetItems(items...)
	l.list.SetSelected(0)
}

// HandleMsg implements Dialog.
func (l *Logout) HandleMsg(msg tea.Msg) Action {
	if kp, ok := msg.(tea.KeyPressMsg); ok {
		if action, handled := l.handleNav(kp); handled {
			return action
		}
		if key.Matches(kp, l.keyMap.Select) {
			pick := l.selected()
			if pick == nil {
				return nil
			}
			registration, ok := l.com.Config().ProviderRegistration(pick.id)
			if !ok || registration.OAuth == nil {
				return nil
			}
			return ActionLogout{Owner: registration.Owner(), AccountNamespace: registration.AccountNamespace, Label: pick.label}
		}
		return l.handleFilter(kp)
	}
	return nil
}

// Cursor implements Dialog.
func (l *Logout) Cursor() *tea.Cursor {
	return InputCursor(l.com.Styles, l.input.Cursor())
}

// Draw implements Dialog.
func (l *Logout) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	return l.draw(scr, area, "Log Out", l)
}

// ShortHelp implements help.KeyMap.
func (l *Logout) ShortHelp() []key.Binding { return l.shortHelp() }

// FullHelp implements help.KeyMap.
func (l *Logout) FullHelp() [][]key.Binding { return l.fullHelp() }

// ActionLogout is emitted when the user picks a provider to log out from.
type ActionLogout struct {
	Owner            providerregistry.RegistrationOwner
	AccountNamespace string
	Label            string
}

// LogoutDoneMsg reports the result of a logout.
type LogoutDoneMsg struct {
	Label string
	Err   error
}

// LogoutCmd removes the stored credentials for a provider.
func LogoutCmd(com *common.Common, action ActionLogout) tea.Cmd {
	return func() tea.Msg {
		if err := validateAccountOwner(com, action.Owner); err != nil {
			return LogoutDoneMsg{Label: action.Label, Err: err}
		}
		if err := com.Workspace.RemoveProviderCredentials(config.ScopeGlobal, action.Owner); err != nil {
			return LogoutDoneMsg{Label: action.Label, Err: err}
		}
		if action.AccountNamespace != "" {
			if err := validateAccountOwner(com, action.Owner); err != nil {
				return LogoutDoneMsg{Label: action.Label, Err: err}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := accounts.RemoveProviderForOwner(ctx, action.AccountNamespace, func() error {
				return validateAccountOwner(com, action.Owner)
			}); err != nil {
				return LogoutDoneMsg{Label: action.Label, Err: err}
			}
		}
		return LogoutDoneMsg{Label: action.Label}
	}
}

// ---------------------------------------------------------------------------
// Login dialog
// ---------------------------------------------------------------------------

// loginState is the login dialog's state machine.
type loginState int

const (
	loginStatePick       loginState = iota
	loginStateBrowser               // waiting for a loopback browser flow
	loginStateDeviceCode            // device flow: code shown, polling
	loginStatePaste                 // gemini: waiting for pasted code
	loginStateSaving
	loginStateError
)

// Login runs OAuth login flows from inside the TUI.
type Login struct {
	accountsPicker
	state   loginState
	spinner spinner.Model

	providerID   string // crux provider id being authenticated
	label        string
	continuation *ActionSelectModel

	// device flow display
	userCode        string
	verificationURL string

	// hosted callback paste flow
	pasteInput textinput.Model
	codeCh     chan string

	cancel context.CancelFunc
	errMsg string

	keyEnter key.Binding
}

var _ Dialog = (*Login)(nil)

// NewLogin creates the login dialog.
func NewLogin(com *common.Common) (*Login, tea.Cmd) {
	l := &Login{accountsPicker: newAccountsPicker(com), state: loginStatePick}
	l.spinner = spinner.New(
		spinner.WithSpinner(spinner.Dot),
		spinner.WithStyle(com.Styles.Dialog.OAuth.Spinner),
	)
	l.pasteInput = textinput.New()
	l.pasteInput.SetVirtualCursor(false)
	l.pasteInput.Placeholder = "Paste authorization code or callback URL"
	l.pasteInput.SetStyles(com.Styles.TextInput)
	l.keyEnter = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "submit"))

	var items []list.FilterableItem
	for _, choice := range oauthProviderChoices(com.Config()) {
		items = append(items, newAccountPickItem(com.Styles, choice.cruxID, choice.label, ""))
	}
	l.list.SetItems(items...)
	l.list.SetSelected(0)
	return l, l.spinner.Tick
}

// NewLoginForProvider starts the registered OAuth flow for one provider without
// presenting the provider picker. It is used by request-time reauthentication.
func NewLoginForProvider(com *common.Common, providerID string) (*Login, tea.Cmd, error) {
	registration, ok := com.Config().ProviderRegistration(providerID)
	if !ok || registration.OAuth == nil {
		return nil, nil, fmt.Errorf("provider %s has no registered OAuth capability", providerID)
	}
	login, _ := NewLogin(com)
	return login, login.start(registration.ProviderID, registration.Name), nil
}

// NewLoginForModel starts registered OAuth and resumes the selected model after
// the credential and account record have been persisted.
func NewLoginForModel(com *common.Common, selection ActionSelectModel) (*Login, tea.Cmd, error) {
	if err := selection.ValidateProviderOwner(com.Config()); err != nil {
		return nil, nil, err
	}
	login, cmd, err := NewLoginForProvider(com, string(selection.Provider.ID))
	if err != nil {
		return nil, nil, err
	}
	login.continuation = &selection
	return login, cmd, nil
}

// ID implements Dialog.
func (l *Login) ID() string { return LoginID }

// loginTokenMsg carries a finished OAuth flow result.
type loginTokenMsg struct {
	registration providerregistry.Registration
	token        *oauth.Token
	err          error
}

// loginDeviceCodeMsg carries device-flow display values.
type loginDeviceCodeMsg struct {
	providerID      string
	userCode        string
	verificationURL string
	poll            tea.Cmd
}

// LoginDoneMsg reports a completed login to the UI for persistence.
type LoginDoneMsg struct {
	Registration providerregistry.Registration
	Token        *oauth.Token
	Continuation *ActionSelectModel
}

func validateAccountOwner(com *common.Common, owner providerregistry.RegistrationOwner) error {
	current, ok := com.Config().ProviderOwner(owner.ProviderID)
	if !ok || current != owner {
		return fmt.Errorf("provider account owner changed for %s", owner.ProviderID)
	}
	return nil
}

// start kicks off the provider-specific flow.
func (l *Login) start(providerID, label string) tea.Cmd {
	l.providerID = providerID
	l.label = label
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel

	registration, ok := l.com.Config().ProviderRegistration(providerID)
	if !ok || registration.OAuth == nil {
		l.state = loginStateError
		l.errMsg = "unsupported OAuth provider: " + providerID
		return nil
	}
	capability := registration.OAuth
	owner := registration.Owner()
	validate := func() error { return validateAccountOwner(l.com, owner) }
	ctx = providertransport.ContextWithOwnerValidator(ctx, validate)
	if err := validate(); err != nil {
		l.state = loginStateError
		l.errMsg = err.Error()
		return nil
	}
	switch capability.Adapter {
	case providerregistry.LoginBrowser:
		if capability.Authorize == nil {
			l.state = loginStateError
			l.errMsg = "OAuth login is declared but its core interpreter is unavailable"
			return nil
		}
		l.state = loginStateBrowser
		return tea.Batch(l.spinner.Tick, func() tea.Msg {
			if err := validateAccountOwner(l.com, owner); err != nil {
				return loginTokenMsg{registration: registration, err: err}
			}
			token, err := capability.Authorize(ctx, func(rawURL string) error {
				return providertransport.OpenURLWithContextOwnerValidator(ctx, browser.OpenURL, rawURL)
			}, nil)
			if err == nil {
				err = validateAccountOwner(l.com, owner)
			}
			return loginTokenMsg{registration: registration, token: token, err: err}
		})
	case providerregistry.LoginHostedPaste:
		if capability.Authorize == nil {
			l.state = loginStateError
			l.errMsg = "OAuth login is declared but its core interpreter is unavailable"
			return nil
		}
		l.state = loginStatePaste
		l.pasteInput.SetValue("")
		l.pasteInput.Focus()
		l.input.Blur()
		l.codeCh = make(chan string, 1)
		codeCh := l.codeCh
		return tea.Batch(l.spinner.Tick, func() tea.Msg {
			if err := validateAccountOwner(l.com, owner); err != nil {
				return loginTokenMsg{registration: registration, err: err}
			}
			token, err := capability.Authorize(ctx, func(rawURL string) error {
				return providertransport.OpenURLWithContextOwnerValidator(ctx, browser.OpenURL, rawURL)
			}, func() (string, error) {
				select {
				case code := <-codeCh:
					return code, nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			})
			if err == nil {
				err = validateAccountOwner(l.com, owner)
			}
			return loginTokenMsg{registration: registration, token: token, err: err}
		})
	case providerregistry.LoginDeviceCode:
		if capability.RequestDeviceCode == nil || capability.PollDeviceCode == nil {
			l.state = loginStateError
			l.errMsg = "device OAuth login is unavailable"
			return nil
		}
		l.state = loginStateBrowser
		return tea.Batch(l.spinner.Tick, func() tea.Msg {
			if err := validateAccountOwner(l.com, owner); err != nil {
				return loginTokenMsg{registration: registration, err: err}
			}
			authorization, err := capability.RequestDeviceCode(ctx)
			if err != nil {
				return loginTokenMsg{registration: registration, err: err}
			}
			if err := validateAccountOwner(l.com, owner); err != nil {
				return loginTokenMsg{registration: registration, err: err}
			}
			if err := providertransport.OpenURLWithContextOwnerValidator(ctx, browser.OpenURL, authorization.VerificationURL); err != nil {
				return loginTokenMsg{registration: registration, err: err}
			}
			return loginDeviceCodeMsg{
				providerID:      providerID,
				userCode:        authorization.UserCode,
				verificationURL: authorization.VerificationURL,
				poll: func() tea.Msg {
					if err := validateAccountOwner(l.com, owner); err != nil {
						return loginTokenMsg{registration: registration, err: err}
					}
					token, err := capability.PollDeviceCode(ctx, authorization)
					if err == nil {
						err = validateAccountOwner(l.com, owner)
					}
					return loginTokenMsg{registration: registration, token: token, err: err}
				},
			}
		})
	default:
		l.state = loginStateError
		l.errMsg = "unsupported OAuth adapter: " + string(capability.Adapter)
		return nil
	}
}

// HandleMsg implements Dialog.
func (l *Login) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		switch l.state {
		case loginStateBrowser, loginStateDeviceCode, loginStatePaste, loginStateSaving:
			var cmd tea.Cmd
			l.spinner, cmd = l.spinner.Update(msg)
			if cmd != nil {
				return ActionCmd{cmd}
			}
		}
	case loginDeviceCodeMsg:
		l.state = loginStateDeviceCode
		l.userCode = msg.userCode
		l.verificationURL = msg.verificationURL
		return ActionCmd{tea.Batch(l.spinner.Tick, msg.poll)}
	case loginTokenMsg:
		if msg.err != nil {
			l.state = loginStateError
			l.errMsg = msg.err.Error()
			return nil
		}
		l.state = loginStateSaving
		return LoginDoneMsg{Registration: msg.registration, Token: msg.token, Continuation: l.continuation}
	case tea.KeyPressMsg:
		switch l.state {
		case loginStatePick:
			if action, handled := l.handleNav(msg); handled {
				return action
			}
			if key.Matches(msg, l.keyMap.Select) {
				pick := l.selected()
				if pick == nil {
					return nil
				}
				if cmd := l.start(pick.id, pick.label); cmd != nil {
					return ActionCmd{cmd}
				}
				return nil
			}
			return l.handleFilter(msg)
		case loginStatePaste:
			switch {
			case key.Matches(msg, l.keyMap.Close):
				l.stop()
				return ActionClose{}
			case key.Matches(msg, l.keyEnter):
				code := strings.TrimSpace(l.pasteInput.Value())
				if code == "" || l.codeCh == nil {
					return nil
				}
				l.codeCh <- code
				l.codeCh = nil
				l.state = loginStateBrowser
				return ActionCmd{l.spinner.Tick}
			default:
				var cmd tea.Cmd
				l.pasteInput, cmd = l.pasteInput.Update(msg)
				return ActionCmd{cmd}
			}
		case loginStateError:
			if key.Matches(msg, l.keyMap.Close) || key.Matches(msg, l.keyEnter) {
				l.stop()
				return ActionClose{}
			}
		default:
			if key.Matches(msg, l.keyMap.Close) {
				l.stop()
				return ActionClose{}
			}
		}
	}
	return nil
}

func (l *Login) stop() {
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
}

// Cursor implements Dialog.
func (l *Login) Cursor() *tea.Cursor {
	switch l.state {
	case loginStatePick:
		return InputCursor(l.com.Styles, l.input.Cursor())
	case loginStatePaste:
		return InputCursor(l.com.Styles, l.pasteInput.Cursor())
	}
	return nil
}

// Draw implements Dialog.
func (l *Login) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	if l.state == loginStatePick {
		return l.draw(scr, area, "Log In", l)
	}

	t := l.com.Styles
	width := max(0, min(accountsDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()

	rc := NewRenderContext(t, width)
	rc.Title = "Log In — " + l.label

	switch l.state {
	case loginStateBrowser, loginStateSaving:
		rc.AddPart(l.spinner.View() + " Waiting for authorization in your browser…")
		rc.AddPart("")
		rc.AddPart(t.Dialog.OAuth.Instructions.Render("Complete the flow in the browser window. Esc to cancel."))
	case loginStateDeviceCode:
		rc.AddPart("Enter this code at " + l.verificationURL + ":")
		rc.AddPart("")
		rc.AddPart(t.Dialog.Title.Render(l.userCode))
		rc.AddPart("")
		rc.AddPart(l.spinner.View() + " Waiting for authorization…")
	case loginStatePaste:
		rc.AddPart("A browser window was opened for provider authorization.")
		rc.AddPart("Paste the authorization code (or callback URL) below:")
		rc.AddPart("")
		l.pasteInput.SetWidth(dialogInputTextWidth(t, l.pasteInput, innerWidth))
		rc.AddPart(t.Dialog.InputPrompt.Render(l.pasteInput.View()))
	case loginStateError:
		rc.AddPart(t.Dialog.OAuth.ErrorText.Render("Login failed:"))
		rc.AddPart("")
		rc.AddPart(wrapText(l.errMsg, innerWidth))
		rc.AddPart("")
		rc.AddPart(t.Dialog.OAuth.Instructions.Render("Press esc to close."))
	}

	view := rc.Render()
	cur := l.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// wrapText hard-wraps s to the given width.
func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	var b strings.Builder
	line := 0
	for _, word := range strings.Fields(s) {
		if line > 0 && line+len(word)+1 > width {
			b.WriteByte('\n')
			line = 0
		} else if line > 0 {
			b.WriteByte(' ')
			line++
		}
		b.WriteString(word)
		line += len(word)
	}
	return b.String()
}

// ShortHelp implements help.KeyMap.
func (l *Login) ShortHelp() []key.Binding {
	if l.state == loginStatePick {
		return l.shortHelp()
	}
	return []key.Binding{l.keyMap.Close}
}

// FullHelp implements help.KeyMap.
func (l *Login) FullHelp() [][]key.Binding {
	if l.state == loginStatePick {
		return l.fullHelp()
	}
	return [][]key.Binding{{l.keyMap.Close}}
}

// SaveLoginCmd persists a completed login: provider config credential plus
// the multi-account store entry (with account identity when resolvable).
func SaveLoginCmd(com *common.Common, msg LoginDoneMsg) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		registration := msg.Registration
		owner := registration.Owner()
		validate := func() error { return validateAccountOwner(com, owner) }
		ctx = providertransport.ContextWithOwnerValidator(ctx, validate)
		if err := validate(); err != nil {
			return LogoutDoneMsg{Label: registration.ProviderID, Err: err}
		}

		displayName := registration.ProviderID
		accountID := ""
		var raw []byte
		if registration.AccountNamespace != "" && registration.Identity != nil {
			if err := validateAccountOwner(com, owner); err != nil {
				return LogoutDoneMsg{Label: registration.ProviderID, Err: err}
			}
			accountID, displayName, raw = registration.Identity(ctx, msg.Token.AccessToken)
			if err := validateAccountOwner(com, owner); err != nil {
				return LogoutDoneMsg{Label: registration.ProviderID, Err: err}
			}
		}
		credential := config.ProviderOAuthCredential{Owner: owner, Token: msg.Token}
		if err := validateAccountOwner(com, owner); err != nil {
			return LogoutDoneMsg{Label: registration.ProviderID, Err: err}
		}
		if err := com.Workspace.SetProviderAPIKey(config.ScopeGlobal, registration.ProviderID, credential); err != nil {
			return LogoutDoneMsg{Label: registration.ProviderID, Err: err}
		}
		if err := validateAccountOwner(com, owner); err != nil {
			return LogoutDoneMsg{Label: registration.ProviderID, Err: err}
		}

		if registration.AccountNamespace != "" {
			if accountID == "" {
				accountID = "default"
			}
			if displayName == "" {
				displayName = accountID
			}
			entry := accounts.FromToken(accountID, displayName, msg.Token, nil)
			entry.Raw = raw
			if err := validateAccountOwner(com, owner); err != nil {
				return LogoutDoneMsg{Label: registration.ProviderID, Err: err}
			}
			if err := accounts.SaveForOwner(ctx, registration.AccountNamespace, entry, func() error {
				return validateAccountOwner(com, owner)
			}); err != nil {
				return AccountSwitchedMsg{CruxProviderID: registration.ProviderID, DisplayName: displayName, Err: err}
			}
			if err := validateAccountOwner(com, owner); err != nil {
				return LogoutDoneMsg{Label: registration.ProviderID, Err: err}
			}
		}
		return AccountSwitchedMsg{CruxProviderID: registration.ProviderID, DisplayName: displayName, Continuation: msg.Continuation}
	}
}
