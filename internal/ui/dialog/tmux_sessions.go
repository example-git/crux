package dialog

import (
	"context"
	"fmt"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/example-git/crux/internal/tmuxsession"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/list"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/sahilm/fuzzy"
)

const (
	TmuxSessionsID              = "tmux_sessions"
	tmuxSessionsDialogMaxWidth  = 72
	tmuxSessionsDialogMaxHeight = 18
)

type tmuxSessionsLoadedMsg struct {
	sessions []tmuxsession.Session
	err      error
}

type TmuxSessions struct {
	com     *common.Common
	help    help.Model
	list    *list.FilterableList
	input   textinput.Model
	loadErr error
	loaded  bool
	keyMap  struct {
		Select   key.Binding
		Next     key.Binding
		Previous key.Binding
		UpDown   key.Binding
		Close    key.Binding
	}
}

type TmuxSessionItem struct {
	*list.Versioned
	session tmuxsession.Session
	t       *styles.Styles
	m       fuzzy.Match
	cache   map[int]string
	focused bool
}

var (
	_ Dialog   = (*TmuxSessions)(nil)
	_ ListItem = (*TmuxSessionItem)(nil)
)

func NewTmuxSessions(com *common.Common) *TmuxSessions {
	value := &TmuxSessions{com: com}
	value.help = help.New()
	value.help.Styles = com.Styles.DialogHelpStyles()
	value.list = list.NewFilterableList()
	value.list.Focus()
	value.input = textinput.New()
	value.input.SetVirtualCursor(false)
	value.input.Placeholder = "Type to filter"
	value.input.SetStyles(com.Styles.TextInput)
	value.input.Focus()
	value.keyMap.Select = key.NewBinding(key.WithKeys("enter", "ctrl+y"), key.WithHelp("enter", "attach"))
	value.keyMap.Next = key.NewBinding(key.WithKeys("down", "ctrl+n"), key.WithHelp("↓", "next item"))
	value.keyMap.Previous = key.NewBinding(key.WithKeys("up", "ctrl+p"), key.WithHelp("↑", "previous item"))
	value.keyMap.UpDown = key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑/↓", "choose"))
	value.keyMap.Close = CloseKey
	return value
}

func (t *TmuxSessions) ID() string {
	return TmuxSessionsID
}

func (t *TmuxSessions) InitialCmd() tea.Cmd {
	return func() tea.Msg {
		sessions, err := tmuxsession.Discover(context.Background())
		return tmuxSessionsLoadedMsg{sessions: sessions, err: err}
	}
}

func (t *TmuxSessions) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tmuxSessionsLoadedMsg:
		t.loaded = true
		t.loadErr = msg.err
		t.setItems(msg.sessions)
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, t.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, t.keyMap.Previous):
			if t.list.IsSelectedFirst() {
				t.list.SelectLast()
			} else {
				t.list.SelectPrev()
			}
			t.list.ScrollToSelected()
		case key.Matches(msg, t.keyMap.Next):
			if t.list.IsSelectedLast() {
				t.list.SelectFirst()
			} else {
				t.list.SelectNext()
			}
			t.list.ScrollToSelected()
		case key.Matches(msg, t.keyMap.Select):
			item, ok := t.list.SelectedItem().(*TmuxSessionItem)
			if ok && item != nil {
				return ActionAttachTmuxSession{Session: item.session}
			}
		default:
			var command tea.Cmd
			t.input, command = t.input.Update(msg)
			t.list.SetFilter(t.input.Value())
			t.list.ScrollToTop()
			t.list.SetSelected(0)
			return ActionCmd{Cmd: command}
		}
	}
	return nil
}

func (t *TmuxSessions) Cursor() *tea.Cursor {
	return InputCursor(t.com.Styles, t.input.Cursor())
}

func (t *TmuxSessions) Draw(screen uv.Screen, area uv.Rectangle) *tea.Cursor {
	styles := t.com.Styles
	width := max(0, min(tmuxSessionsDialogMaxWidth, area.Dx()-styles.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(tmuxSessionsDialogMaxHeight, area.Dy()-styles.Dialog.View.GetVerticalBorderSize()))
	innerWidth := width - styles.Dialog.View.GetHorizontalFrameSize()
	heightOffset := styles.Dialog.Title.GetVerticalFrameSize() + titleContentHeight + styles.Dialog.InputPrompt.GetVerticalFrameSize() + inputContentHeight + styles.Dialog.HelpView.GetVerticalFrameSize() + styles.Dialog.View.GetVerticalFrameSize()
	t.input.SetWidth(dialogInputTextWidth(styles, t.input, innerWidth))
	t.list.SetSize(innerWidth, max(0, height-heightOffset))
	renderContext := NewRenderContext(styles, width)
	renderContext.Title = "Crux tmux Sessions"
	renderContext.AddPart(styles.Dialog.InputPrompt.Render(t.input.View()))
	switch {
	case t.loadErr != nil:
		renderContext.AddPart(styles.Dialog.PrimaryText.Render(t.loadErr.Error()))
	case t.loaded && len(t.list.FilteredItems()) == 0:
		renderContext.AddPart(styles.Dialog.SecondaryText.Render("No active Crux tmux sessions"))
	default:
		renderContext.AddPart(styles.Dialog.List.Height(t.list.Height()).Render(t.list.Render()))
	}
	renderContext.Help = renderDialogHelp(styles, &t.help, t, innerWidth)
	view := renderContext.Render()
	cursor := t.Cursor()
	DrawCenterCursor(screen, area, view, cursor)
	return cursor
}

func (t *TmuxSessions) ShortHelp() []key.Binding {
	return []key.Binding{t.keyMap.UpDown, t.keyMap.Select, t.keyMap.Close}
}

func (t *TmuxSessions) FullHelp() [][]key.Binding {
	return [][]key.Binding{{t.keyMap.Select, t.keyMap.Next, t.keyMap.Previous, t.keyMap.Close}}
}

func (t *TmuxSessions) setItems(sessions []tmuxsession.Session) {
	items := make([]list.FilterableItem, 0, len(sessions))
	for _, session := range sessions {
		items = append(items, &TmuxSessionItem{Versioned: list.NewVersioned(), session: session, t: t.com.Styles})
	}
	t.list.SetItems(items...)
	t.list.SetSelected(0)
}

func (t *TmuxSessionItem) Finished() bool {
	return true
}

func (t *TmuxSessionItem) Filter() string {
	return t.session.Name + " " + t.socketLabel()
}

func (t *TmuxSessionItem) ID() string {
	return t.socketLabel() + ":" + t.session.ID
}

func (t *TmuxSessionItem) SetFocused(focused bool) {
	if t.focused == focused {
		return
	}
	t.focused = focused
	t.cache = nil
	t.Bump()
}

func (t *TmuxSessionItem) SetMatch(match fuzzy.Match) {
	if sameFuzzyMatch(t.m, match) {
		return
	}
	t.m = match
	t.cache = nil
	t.Bump()
}

func (t *TmuxSessionItem) Render(width int) string {
	info := fmt.Sprintf("%s · %d windows · %d attached", t.socketLabel(), t.session.Windows, t.session.Attached)
	itemStyles := ListItemStyles{ItemBlurred: t.t.Dialog.NormalItem, ItemFocused: t.t.Dialog.SelectedItem, InfoTextBlurred: t.t.Dialog.ListItem.InfoBlurred, InfoTextFocused: t.t.Dialog.ListItem.InfoFocused}
	return renderItem(itemStyles, t.session.Name, info, t.focused, width, t.cache, &t.m)
}

func (t *TmuxSessionItem) socketLabel() string {
	if t.session.Socket == "" {
		return "default"
	}
	return t.session.Socket
}
