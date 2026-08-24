package dialog

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
)

const InstructionsPreviewID = "instructions-preview"

const (
	instructionsPreviewSectionPaneWidth = 24
	instructionsPreviewTopPadding       = 2
)

type InstructionPreviewSection struct {
	ID         string
	Label      string
	Content    string
	Disabled   bool
	Toggleable bool
}

type previewRenderKey struct {
	width    int
	markdown bool
}

type instructionsPreviewRender struct {
	content      string
	sectionLines []int
}

type instructionsPreviewRenderedMsg struct {
	generation uint64
	key        previewRenderKey
	render     instructionsPreviewRender
}

type InstructionsPreview struct {
	com              *common.Common
	markdown         bool
	viewport         viewport.Model
	keyMap           instructionsPreviewKeyMap
	sections         []InstructionPreviewSection
	sectionCursor    int
	sectionsFocused  bool
	rendered         map[previewRenderKey]instructionsPreviewRender
	viewportKey      previewRenderKey
	renderGeneration uint64
	initialWidth     int
}

type instructionsPreviewKeyMap struct {
	Close         key.Binding
	Toggle        key.Binding
	FocusSections key.Binding
	FocusContent  key.Binding
	Select        key.Binding
	ToggleSection key.Binding
	Up            key.Binding
	Down          key.Binding
}

var _ Dialog = (*InstructionsPreview)(nil)

func NewInstructionsPreview(com *common.Common, sections []InstructionPreviewSection, width int) *InstructionsPreview {
	vp := viewport.New()
	keyMap := instructionsPreviewKeyMap{
		Close:         CloseKey,
		Toggle:        key.NewBinding(key.WithKeys("tab", "m"), key.WithHelp("tab/m", "toggle format")),
		FocusSections: key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "sections")),
		FocusContent:  key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "content")),
		Select:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "jump")),
		ToggleSection: key.NewBinding(key.WithKeys(" ", "space"), key.WithHelp("space", "enable/disable")),
		Up:            key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:          key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	}
	vp.KeyMap = viewport.KeyMap{
		Up:           keyMap.Up,
		Down:         keyMap.Down,
		PageUp:       key.NewBinding(key.WithKeys("pgup")),
		PageDown:     key.NewBinding(key.WithKeys("pgdown")),
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
	}
	return &InstructionsPreview{
		com:          com,
		markdown:     true,
		viewport:     vp,
		keyMap:       keyMap,
		sections:     sections,
		rendered:     make(map[previewRenderKey]instructionsPreviewRender),
		initialWidth: width,
	}
}

func (*InstructionsPreview) ID() string { return InstructionsPreviewID }

func (d *InstructionsPreview) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, d.keyMap.Toggle):
			d.markdown = !d.markdown
			d.viewport.SetYOffset(0)
			return ActionCmd{Cmd: d.renderCmd(d.contentWidth(d.initialWidth), d.markdown)}
		case key.Matches(msg, d.keyMap.FocusSections):
			d.sectionsFocused = true
		case key.Matches(msg, d.keyMap.FocusContent):
			d.sectionsFocused = false
		case d.sectionsFocused && key.Matches(msg, d.keyMap.Up):
			d.moveSection(-1)
		case d.sectionsFocused && key.Matches(msg, d.keyMap.Down):
			d.moveSection(1)
		case d.sectionsFocused && key.Matches(msg, d.keyMap.Select):
			d.jumpToSection()
		case key.Matches(msg, d.keyMap.ToggleSection):
			return d.toggleSection()
		}
	case common.CoalescedWheelMsg:
		d.viewport, _ = d.viewport.Update(tea.MouseWheelMsg(msg.Mouse))
		return nil
	case instructionsPreviewRenderedMsg:
		if msg.generation != d.renderGeneration {
			return nil
		}
		d.rendered[msg.key] = msg.render
		if msg.key.width == d.viewport.Width() && msg.key.markdown == d.markdown {
			d.viewport.SetContentLines(previewViewportLines(msg.render.content, d.viewport.Height()))
			d.viewportKey = msg.key
		}
	}
	if !d.sectionsFocused {
		d.viewport, _ = d.viewport.Update(msg)
	}
	return nil
}

func (d *InstructionsPreview) moveSection(delta int) {
	if len(d.sections) == 0 {
		return
	}
	d.sectionCursor = min(max(d.sectionCursor+delta, 0), len(d.sections)-1)
	d.jumpToSection()
}

func (d *InstructionsPreview) jumpToSection() {
	render, ok := d.rendered[d.viewportKey]
	if !ok || d.sectionCursor >= len(render.sectionLines) || render.sectionLines[d.sectionCursor] < 0 {
		return
	}
	d.viewport.SetYOffset(instructionsPreviewTopPadding + render.sectionLines[d.sectionCursor])
}

func (d *InstructionsPreview) toggleSection() Action {
	if d.sectionCursor >= len(d.sections) || !d.sections[d.sectionCursor].Toggleable {
		return nil
	}
	section := &d.sections[d.sectionCursor]
	section.Disabled = !section.Disabled
	d.rendered = make(map[previewRenderKey]instructionsPreviewRender)
	d.viewport.SetYOffset(0)
	return ActionPreviewInstructionSectionToggled{
		ID:       section.ID,
		Disabled: section.Disabled,
		Cmd:      d.renderCmd(d.contentWidth(d.initialWidth), d.markdown),
	}
}

func (d *InstructionsPreview) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := min(max(area.Dx()-4, 1), 120)
	height := max(min(area.Dy()-4, 40), 1)
	paneWidth := 0
	if len(d.sections) > 1 && width >= 64 {
		paneWidth = instructionsPreviewSectionPaneWidth
	}
	contentWidth := max(width-t.Dialog.View.GetHorizontalFrameSize()-2-paneWidth, 1)
	viewportHeight := max(height-t.Dialog.View.GetVerticalFrameSize()-4, 1)
	d.viewport.SetWidth(contentWidth)
	d.viewport.SetHeight(viewportHeight)
	renderKey := previewRenderKey{width: contentWidth, markdown: d.markdown}
	if rendered, ok := d.rendered[renderKey]; ok && d.viewportKey != renderKey {
		d.viewport.SetContentLines(previewViewportLines(rendered.content, viewportHeight))
		d.viewportKey = renderKey
	}

	format := "Markdown"
	if !d.markdown {
		format = "Text"
	}
	header := t.Dialog.TitleText.Render("Instruction Preview")
	mode := t.Dialog.SecondaryText.Render("  " + format)
	hint := t.Dialog.SecondaryText.Render("space: enable/disable · tab/m: format · ←/→: focus · esc: back")
	content := d.viewport.View()
	if _, ok := d.rendered[renderKey]; !ok {
		content = t.Dialog.SecondaryText.Render("Rendering preview…")
	}
	content = lipgloss.NewStyle().Width(contentWidth).Height(viewportHeight).MaxHeight(viewportHeight).Render(content)
	if paneWidth > 0 {
		content = lipgloss.JoinHorizontal(
			lipgloss.Top,
			d.sectionsView(t, paneWidth, viewportHeight),
			"  ",
			content,
		)
	}
	body := lipgloss.JoinVertical(lipgloss.Left, header+mode, "", content, "", hint)
	DrawCenter(scr, area, t.Dialog.View.Width(width).Render(body))
	return nil
}

func previewViewportLines(content string, height int) []string {
	lines := make([]string, instructionsPreviewTopPadding, instructionsPreviewTopPadding+previewLineCount(content)+max(height-1, 0))
	lines = append(lines, strings.Split(content, "\n")...)
	lines = append(lines, make([]string, max(height-1, 0))...)
	return lines
}

func (d *InstructionsPreview) sectionsView(t *styles.Styles, width, height int) string {
	rows := []string{t.Dialog.PrimaryText.Bold(true).Render("Sections")}
	visible := max(height-1, 0)
	start := 0
	if d.sectionCursor >= visible && visible > 0 {
		start = d.sectionCursor - visible + 1
	}
	end := min(start+visible, len(d.sections))
	for index := start; index < end; index++ {
		section := d.sections[index]
		title := ansi.Truncate(section.Label, max(width-6, 1), "…")
		check := "✓"
		if section.Disabled {
			check = " "
		}
		label := "  [" + check + "] " + title
		if index == d.sectionCursor {
			label = "▸ [" + check + "] " + title
			if d.sectionsFocused && !section.Disabled {
				rows = append(rows, t.Dialog.SelectedItem.Width(width).Render(label))
				continue
			}
		}
		if section.Disabled {
			rows = append(rows, t.Dialog.SecondaryText.Width(width).Render(label))
			continue
		}
		rows = append(rows, t.Dialog.NormalItem.Width(width).Render(label))
	}
	return lipgloss.NewStyle().Width(width).Height(height).MaxHeight(height).Render(
		lipgloss.JoinVertical(lipgloss.Left, rows...),
	)
}

func (d *InstructionsPreview) StartLoading() tea.Cmd {
	return d.renderCmd(d.contentWidth(d.initialWidth), d.markdown)
}

func (d *InstructionsPreview) StopLoading() {}

func (d *InstructionsPreview) contentWidth(width int) int {
	dialogWidth := min(max(width-4, 1), 120)
	paneWidth := 0
	if len(d.sections) > 1 && dialogWidth >= 64 {
		paneWidth = instructionsPreviewSectionPaneWidth
	}
	return max(dialogWidth-d.com.Styles.Dialog.View.GetHorizontalFrameSize()-2-paneWidth, 1)
}

func (d *InstructionsPreview) renderCmd(width int, markdown bool) tea.Cmd {
	d.renderGeneration++
	generation := d.renderGeneration
	sections := append([]InstructionPreviewSection(nil), d.sections...)
	previewStyles := d.com.Styles
	return func() tea.Msg {
		return instructionsPreviewRenderedMsg{
			generation: generation,
			key:        previewRenderKey{width: width, markdown: markdown},
			render:     renderInstructionsPreviewSections(previewStyles, sections, width, markdown),
		}
	}
}

func renderInstructionsPreviewSections(previewStyles *styles.Styles, sections []InstructionPreviewSection, width int, markdown bool) instructionsPreviewRender {
	if len(sections) == 0 {
		return instructionsPreviewRender{content: previewStyles.Dialog.SecondaryText.Render("No active instructions.")}
	}

	parts := make([]string, 0, len(sections))
	sectionLines := make([]int, len(sections))
	for index := range sectionLines {
		sectionLines[index] = -1
	}
	line := 0
	if !markdown {
		for index, section := range sections {
			if section.Disabled {
				continue
			}
			if len(parts) > 0 {
				line += 2
			}
			sectionLines[index] = line
			content := previewWrappedContent(strings.TrimSpace(section.Content), width)
			parts = append(parts, content)
			line += strings.Count(content, "\n")
		}
		content := strings.Join(parts, "\n\n")
		if content == "" {
			content = previewStyles.Dialog.SecondaryText.Render("No active instructions.")
		}
		return instructionsPreviewRender{
			content:      content,
			sectionLines: sectionLines,
		}
	}

	renderer := common.MarkdownRenderer(previewStyles, width)
	lock := common.LockMarkdownRenderer(renderer)
	lock.Lock()
	defer lock.Unlock()
	for index, section := range sections {
		if section.Disabled {
			continue
		}
		if len(parts) > 0 {
			line += 2
		}
		sectionLines[index] = line
		content, err := renderer.Render(section.Content)
		if err != nil {
			content = section.Content
		}
		content = strings.TrimSuffix(content, "\n")
		parts = append(parts, content)
		line += strings.Count(content, "\n")
	}
	content := strings.Join(parts, "\n\n")
	if content == "" {
		content = previewStyles.Dialog.SecondaryText.Render("No active instructions.")
	}
	return instructionsPreviewRender{
		content:      content,
		sectionLines: sectionLines,
	}
}

func previewWrappedContent(content string, width int) string {
	return ansi.Hardwrap(content, max(width, 1), true)
}

func previewLineCount(content string) int {
	return strings.Count(content, "\n") + 1
}
