package dialog

import (
	"context"
	"fmt"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/list"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/sahilm/fuzzy"
)

const (
	ProjectsID              = "projects"
	projectsDialogMaxWidth  = 64
	projectsDialogMaxHeight = 18
)

type projectsLoadedMsg struct {
	projects []proto.ProjectInfo
	err      error
}

type Projects struct {
	com     *common.Common
	help    help.Model
	list    *list.FilterableList
	input   textinput.Model
	loadErr error
	keyMap  struct {
		Select   key.Binding
		Next     key.Binding
		Previous key.Binding
		UpDown   key.Binding
		Close    key.Binding
	}
}

type ProjectItem struct {
	*list.Versioned
	project proto.ProjectInfo
	disable bool
	t       *styles.Styles
	m       fuzzy.Match
	cache   map[int]string
	focused bool
}

var (
	_ Dialog   = (*Projects)(nil)
	_ ListItem = (*ProjectItem)(nil)
)

func NewProjects(com *common.Common) *Projects {
	projectDialog := &Projects{com: com}
	projectDialog.help = help.New()
	projectDialog.help.Styles = com.Styles.DialogHelpStyles()
	projectDialog.list = list.NewFilterableList()
	projectDialog.list.Focus()
	projectDialog.input = textinput.New()
	projectDialog.input.SetVirtualCursor(false)
	projectDialog.input.Placeholder = "Type to filter"
	projectDialog.input.SetStyles(com.Styles.TextInput)
	projectDialog.input.Focus()
	projectDialog.keyMap.Select = key.NewBinding(key.WithKeys("enter", "ctrl+y"), key.WithHelp("enter", "confirm"))
	projectDialog.keyMap.Next = key.NewBinding(key.WithKeys("down", "ctrl+n"), key.WithHelp("↓", "next item"))
	projectDialog.keyMap.Previous = key.NewBinding(key.WithKeys("up", "ctrl+p"), key.WithHelp("↑", "previous item"))
	projectDialog.keyMap.UpDown = key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑/↓", "choose"))
	projectDialog.keyMap.Close = CloseKey
	projectDialog.setItems(nil)
	return projectDialog
}

func (p *Projects) ID() string {
	return ProjectsID
}

func (p *Projects) InitialCmd() tea.Cmd {
	return func() tea.Msg {
		entries, err := p.com.Workspace.ListProjects(context.Background())
		return projectsLoadedMsg{projects: entries, err: err}
	}
}

func (p *Projects) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case projectsLoadedMsg:
		p.loadErr = msg.err
		p.setItems(msg.projects)
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, p.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, p.keyMap.Previous):
			if p.list.IsSelectedFirst() {
				p.list.SelectLast()
			} else {
				p.list.SelectPrev()
			}
			p.list.ScrollToSelected()
		case key.Matches(msg, p.keyMap.Next):
			if p.list.IsSelectedLast() {
				p.list.SelectFirst()
			} else {
				p.list.SelectNext()
			}
			p.list.ScrollToSelected()
		case key.Matches(msg, p.keyMap.Select):
			item, ok := p.list.SelectedItem().(*ProjectItem)
			if !ok || item == nil || (!item.disable && item.project.Status == "completed") {
				break
			}
			return ActionSelectProject{Slug: item.project.Slug}
		default:
			var cmd tea.Cmd
			p.input, cmd = p.input.Update(msg)
			p.list.SetFilter(p.input.Value())
			p.list.ScrollToTop()
			p.list.SetSelected(0)
			return ActionCmd{Cmd: cmd}
		}
	}
	return nil
}

func (p *Projects) Cursor() *tea.Cursor {
	return InputCursor(p.com.Styles, p.input.Cursor())
}

func (p *Projects) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := p.com.Styles
	width := max(0, min(projectsDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(projectsDialogMaxHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	innerWidth := width - t.Dialog.View.GetHorizontalFrameSize()
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight + t.Dialog.InputPrompt.GetVerticalFrameSize() + inputContentHeight + t.Dialog.HelpView.GetVerticalFrameSize() + t.Dialog.View.GetVerticalFrameSize()
	p.input.SetWidth(dialogInputTextWidth(t, p.input, innerWidth))
	p.list.SetSize(innerWidth, max(0, height-heightOffset))
	renderContext := NewRenderContext(t, width)
	renderContext.Title = "Projects"
	renderContext.AddPart(t.Dialog.InputPrompt.Render(p.input.View()))
	if p.loadErr != nil {
		renderContext.AddPart(t.Dialog.PrimaryText.Render(p.loadErr.Error()))
	} else {
		renderContext.AddPart(t.Dialog.List.Height(p.list.Height()).Render(p.list.Render()))
	}
	renderContext.Help = renderDialogHelp(t, &p.help, p, innerWidth)
	view := renderContext.Render()
	cursor := p.Cursor()
	DrawCenterCursor(scr, area, view, cursor)
	return cursor
}

func (p *Projects) ShortHelp() []key.Binding {
	return []key.Binding{p.keyMap.UpDown, p.keyMap.Select, p.keyMap.Close}
}

func (p *Projects) FullHelp() [][]key.Binding {
	return [][]key.Binding{{p.keyMap.Select, p.keyMap.Next, p.keyMap.Previous, p.keyMap.Close}}
}

func (p *Projects) setItems(projects []proto.ProjectInfo) {
	items := make([]list.FilterableItem, 0, len(projects)+1)
	selectedIndex := 0
	hasSelection := false
	for _, project := range projects {
		if project.Selected {
			hasSelection = true
			break
		}
	}
	items = append(items, &ProjectItem{Versioned: list.NewVersioned(), disable: true, t: p.com.Styles, project: proto.ProjectInfo{Name: "Disabled", Selected: !hasSelection}})
	for _, project := range projects {
		if project.Selected {
			selectedIndex = len(items)
		}
		items = append(items, &ProjectItem{Versioned: list.NewVersioned(), project: project, t: p.com.Styles})
	}
	p.list.SetItems(items...)
	p.list.SetSelected(selectedIndex)
	p.list.ScrollToSelected()
}

func (p *ProjectItem) Finished() bool {
	return true
}

func (p *ProjectItem) Filter() string {
	return p.project.Name + " " + p.project.Slug
}

func (p *ProjectItem) ID() string {
	if p.disable {
		return "disabled"
	}
	return p.project.Slug
}

func (p *ProjectItem) SetFocused(focused bool) {
	if p.focused == focused {
		return
	}
	p.focused = focused
	p.cache = nil
	p.Bump()
}

func (p *ProjectItem) SetMatch(match fuzzy.Match) {
	if sameFuzzyMatch(p.m, match) {
		return
	}
	p.m = match
	p.cache = nil
	p.Bump()
}

func (p *ProjectItem) Render(width int) string {
	info := fmt.Sprintf("%d/%d", p.project.Completed, p.project.Total)
	if p.disable {
		info = "no project context"
	} else if p.project.Status == "completed" {
		info = "completed"
	} else if p.project.Selected {
		info = "current · " + info
	}
	itemStyles := ListItemStyles{ItemBlurred: p.t.Dialog.NormalItem, ItemFocused: p.t.Dialog.SelectedItem, InfoTextBlurred: p.t.Dialog.ListItem.InfoBlurred, InfoTextFocused: p.t.Dialog.ListItem.InfoFocused}
	return renderItem(itemStyles, p.project.Name, info, p.focused, width, p.cache, &p.m)
}
