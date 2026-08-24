package dialog

import (
	"context"
	"errors"
	"slices"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/list"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/sahilm/fuzzy"
)

const (
	// RewindID is the identifier for the rewind dialog.
	RewindID              = "rewind"
	rewindDialogMaxWidth  = 70
	rewindDialogMinHeight = 8
	rewindDialogMaxHeight = 20
)

type rewindMode uint8

const (
	rewindModeSelectMessage rewindMode = iota
	rewindModeSelectAction
)

// ActionRewind is sent when the user confirms a rewind.
type ActionRewind struct {
	SessionID string
	MessageID string
	Text      string
	Summarize bool
}

// Rewind is a dialog for rewinding a session to a prior user message.
type Rewind struct {
	com       *common.Common
	help      help.Model
	list      *list.FilterableList
	input     textinput.Model
	sessionID string
	mode      rewindMode

	selected message.Message

	keyMap struct {
		Select   key.Binding
		Next     key.Binding
		Previous key.Binding
		UpDown   key.Binding
		Back     key.Binding
		Close    key.Binding
	}
}

// RewindItem represents a user message in the rewind dialog list.
type RewindItem struct {
	*list.Versioned
	msg     message.Message
	title   string
	t       *styles.Styles
	m       fuzzy.Match
	cache   map[int]string
	focused bool
}

// rewindActionItem represents one of the two rewind actions.
type rewindActionItem struct {
	*list.Versioned
	id        string
	title     string
	summarize bool
	t         *styles.Styles
	m         fuzzy.Match
	cache     map[int]string
	focused   bool
}

var (
	_ Dialog   = (*Rewind)(nil)
	_ ListItem = (*RewindItem)(nil)
	_ ListItem = (*rewindActionItem)(nil)
)

// NewRewind creates a new rewind dialog for the given session.
func NewRewind(com *common.Common, sessionID string) (*Rewind, error) {
	r := &Rewind{com: com, sessionID: sessionID}

	msgs, err := com.Workspace.ListUserMessages(context.TODO(), sessionID)
	if err != nil {
		return nil, err
	}

	items := make([]list.FilterableItem, 0, len(msgs))
	for _, msg := range slices.Backward(msgs) {
		text := strings.TrimSpace(msg.Content().Text)
		if text == "" {
			continue
		}
		title, _, _ := strings.Cut(text, "\n")
		items = append(items, &RewindItem{
			Versioned: list.NewVersioned(),
			msg:       msg,
			title:     title,
			t:         com.Styles,
		})
	}
	if len(items) == 0 {
		return nil, errors.New("no user messages to rewind to")
	}

	help := help.New()
	help.Styles = com.Styles.DialogHelpStyles()
	r.help = help

	r.list = list.NewFilterableList(items...)
	r.list.Focus()
	r.list.SetSelected(0)

	r.input = textinput.New()
	r.input.SetVirtualCursor(false)
	r.input.Placeholder = "Type to filter"
	r.input.SetStyles(com.Styles.TextInput)
	r.input.Focus()

	r.keyMap.Select = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "choose"),
	)
	r.keyMap.Next = key.NewBinding(
		key.WithKeys("down", "ctrl+n"),
		key.WithHelp("↓", "next item"),
	)
	r.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	r.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑/↓", "choose"),
	)
	r.keyMap.Back = key.NewBinding(
		key.WithKeys("esc"),
		key.WithHelp("esc", "back"),
	)
	r.keyMap.Close = CloseKey

	return r, nil
}

// ID implements Dialog.
func (r *Rewind) ID() string {
	return RewindID
}

// HandleMsg implements [Dialog].
func (r *Rewind) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, r.keyMap.Close):
			if r.mode == rewindModeSelectAction {
				r.showMessageList()
				return nil
			}
			return ActionClose{}
		case key.Matches(msg, r.keyMap.Previous):
			r.list.Focus()
			if r.list.IsSelectedFirst() {
				r.list.SelectLast()
			} else {
				r.list.SelectPrev()
			}
			r.list.ScrollToSelected()
		case key.Matches(msg, r.keyMap.Next):
			r.list.Focus()
			if r.list.IsSelectedLast() {
				r.list.SelectFirst()
			} else {
				r.list.SelectNext()
			}
			r.list.ScrollToSelected()
		case key.Matches(msg, r.keyMap.Select):
			item := r.list.SelectedItem()
			if item == nil {
				break
			}
			switch it := item.(type) {
			case *RewindItem:
				r.selected = it.msg
				r.showActionList()
			case *rewindActionItem:
				return ActionRewind{
					SessionID: r.sessionID,
					MessageID: r.selected.ID,
					Text:      r.selected.Content().Text,
					Summarize: it.summarize,
				}
			}
		default:
			if r.mode != rewindModeSelectMessage {
				break
			}
			var cmd tea.Cmd
			r.input, cmd = r.input.Update(msg)
			r.list.SetFilter(r.input.Value())
			r.list.ScrollToTop()
			r.list.SetSelected(0)
			return ActionCmd{cmd}
		}
	}
	return nil
}

func (r *Rewind) showActionList() {
	r.mode = rewindModeSelectAction
	r.list.SetFilter("")
	r.list.SetItems(
		&rewindActionItem{
			Versioned: list.NewVersioned(),
			id:        "rewind",
			title:     "Rewind (delete messages)",
			t:         r.com.Styles,
		},
		&rewindActionItem{
			Versioned: list.NewVersioned(),
			id:        "summarize",
			title:     "Summarize, then rewind",
			summarize: true,
			t:         r.com.Styles,
		},
	)
	r.list.SetSelected(0)
	r.list.ScrollToTop()
}

func (r *Rewind) showMessageList() {
	rw, err := NewRewind(r.com, r.sessionID)
	if err != nil {
		return
	}
	r.mode = rewindModeSelectMessage
	r.input.SetValue("")
	r.list = rw.list
	r.list.Focus()
}

// Cursor returns the cursor position relative to the dialog.
func (r *Rewind) Cursor() *tea.Cursor {
	if r.mode != rewindModeSelectMessage {
		return nil
	}
	return InputCursor(r.com.Styles, r.input.Cursor())
}

// Draw implements [Dialog].
func (r *Rewind) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := r.com.Styles
	width := max(0, min(rewindDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()

	r.input.SetWidth(dialogInputTextWidth(t, r.input, innerWidth))

	listTotalHeight := r.list.TotalHeight()
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.InputPrompt.GetVerticalFrameSize() + inputContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() +
		t.Dialog.View.GetVerticalFrameSize()
	desiredHeight := heightOffset + listTotalHeight
	maxAvailable := area.Dy() - t.Dialog.View.GetVerticalBorderSize()
	height := max(rewindDialogMinHeight, min(rewindDialogMaxHeight, desiredHeight, maxAvailable))

	listHeight, listTotalHeight, _ := sizeDialogList(t, r.list, innerWidth, height)

	rc := NewRenderContext(t, width)
	var cur *tea.Cursor
	switch r.mode {
	case rewindModeSelectAction:
		rc.Title = "Rewind How?"
	default:
		rc.Title = "Rewind to Message"
		inputView := t.Dialog.InputPrompt.Render(r.input.View())
		cur = r.Cursor()
		rc.AddPart(inputView)
	}

	visibleCount := len(r.list.FilteredItems())
	if r.list.Height() >= visibleCount {
		r.list.ScrollToTop()
	} else {
		r.list.ScrollToSelected()
	}

	listView := t.Dialog.List.Height(r.list.Height()).Render(r.list.Render())
	listView = joinScrollbar(t, listView, listHeight, listTotalHeight, listHeight, r.list.Offset())
	rc.AddPart(listView)
	rc.Help = renderDialogHelp(t, &r.help, r, innerWidth)

	view := rc.Render()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// ShortHelp implements [help.KeyMap].
func (r *Rewind) ShortHelp() []key.Binding {
	return []key.Binding{
		r.keyMap.UpDown,
		r.keyMap.Select,
		r.keyMap.Close,
	}
}

// FullHelp implements [help.KeyMap].
func (r *Rewind) FullHelp() [][]key.Binding {
	return [][]key.Binding{{
		r.keyMap.Select,
		r.keyMap.Next,
		r.keyMap.Previous,
		r.keyMap.Close,
	}}
}

// Filter implements list.FilterableItem.
func (r *RewindItem) Filter() string { return r.title }

// ID implements list.Item.
func (r *RewindItem) ID() string { return r.msg.ID }

// Finished implements list.Item.
func (r *RewindItem) Finished() bool { return true }

// SetFocused implements ListItem.
func (r *RewindItem) SetFocused(focused bool) {
	if r.focused == focused {
		return
	}
	r.cache = nil
	r.focused = focused
	if r.Versioned != nil {
		r.Bump()
	}
}

// SetMatch implements ListItem.
func (r *RewindItem) SetMatch(m fuzzy.Match) {
	if sameFuzzyMatch(r.m, m) {
		return
	}
	r.cache = nil
	r.m = m
	if r.Versioned != nil {
		r.Bump()
	}
}

// Render implements list.Item.
func (r *RewindItem) Render(width int) string {
	styles := ListItemStyles{
		ItemBlurred:     r.t.Dialog.NormalItem,
		ItemFocused:     r.t.Dialog.SelectedItem,
		InfoTextBlurred: r.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: r.t.Dialog.ListItem.InfoFocused,
	}
	return renderItem(styles, r.title, "", r.focused, width, r.cache, &r.m)
}

// Filter implements list.FilterableItem.
func (r *rewindActionItem) Filter() string { return r.title }

// ID implements list.Item.
func (r *rewindActionItem) ID() string { return r.id }

// Finished implements list.Item.
func (r *rewindActionItem) Finished() bool { return true }

// SetFocused implements ListItem.
func (r *rewindActionItem) SetFocused(focused bool) {
	if r.focused == focused {
		return
	}
	r.cache = nil
	r.focused = focused
	if r.Versioned != nil {
		r.Bump()
	}
}

// SetMatch implements ListItem.
func (r *rewindActionItem) SetMatch(m fuzzy.Match) {
	if sameFuzzyMatch(r.m, m) {
		return
	}
	r.cache = nil
	r.m = m
	if r.Versioned != nil {
		r.Bump()
	}
}

// Render implements list.Item.
func (r *rewindActionItem) Render(width int) string {
	styles := ListItemStyles{
		ItemBlurred:     r.t.Dialog.NormalItem,
		ItemFocused:     r.t.Dialog.SelectedItem,
		InfoTextBlurred: r.t.Dialog.ListItem.InfoBlurred,
		InfoTextFocused: r.t.Dialog.ListItem.InfoFocused,
	}
	return renderItem(styles, r.title, "", r.focused, width, r.cache, &r.m)
}
