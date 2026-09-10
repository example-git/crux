package dialog

import (
	"image"
	"maps"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/foundation/bubbles/key"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
)

// YesNo is an inline yes/no confirmation component. For open-ended
// responses, use FreeText instead. Notes can be added via alt+n.
type YesNo struct {
	questionEditor
	Request    question.Question
	selectedNo bool
	focused    bool
	compositor *lipgloss.Compositor
	hoverX     int
	hoverY     int

	keyLeftRight key.Binding
	keyEnter     key.Binding
	keyYes       key.Binding
	keyNo        key.Binding
	keyClose     key.Binding

	lastResponse    question.Answer
	lastWidth       int
	scrollOffset    int
	viewportHeight  int
	contentHeight   int
	viewportRect    uv.Rectangle
	revealSelection bool
	wheelActive     bool
}

// NewYesNo creates a new inline yes/no question component.
func NewYesNo(sty *styles.Styles, req question.Question) *YesNo {
	return &YesNo{
		questionEditor: newQuestionEditor(sty),
		Request:        req,
		selectedNo:     true, // Default to "No" for safety.
		keyLeftRight:   key.NewBinding(key.WithKeys("left", "right", "h", "l"), key.WithHelp("←/→", "switch")),
		keyEnter:       key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "confirm")),
		keyYes:         key.NewBinding(key.WithKeys("y", "Y"), key.WithHelp("y", "yes")),
		keyNo:          key.NewBinding(key.WithKeys("n", "N"), key.WithHelp("n", "no")),
		keyClose:       CloseKey,
	}
}

// HandleKey processes a key press. Returns true when the user has
// made a choice or dismissed the question.
func (d *YesNo) HandleKey(msg tea.KeyPressMsg) (bool, tea.Cmd) {
	// Note editor takes priority when active.
	if d.activeNoteKey != "" && d.noteEditor.Focused() {
		d.wheelActive = false
		cmd, handled := d.handleNoteKey(msg, d.keyClose, func() { d.closeNote("_question") })
		if handled {
			return false, cmd
		}
	}

	switch {
	case key.Matches(msg, CloseKey):
		d.answer(question.Answer{QuestionID: d.Request.ID})
		return true, nil
	case key.Matches(msg, d.keyLeftRight):
		d.selectedNo = !d.selectedNo
		d.revealSelection = true
		return false, nil
	case key.Matches(msg, d.keyEnter):
		d.answer(d.respond(!d.selectedNo))
		return true, nil
	case key.Matches(msg, d.keyYes):
		d.answer(d.respond(true))
		return true, nil
	case key.Matches(msg, d.keyNo):
		d.answer(d.respond(false))
		return true, nil
	case key.Matches(msg, d.keyNote):
		d.wheelActive = false
		return false, d.openNote("_question")
	}
	switch msg.String() {
	case "up":
		d.HandleWheel(0, -1)
	case "down":
		d.HandleWheel(0, 1)
	case "pgup":
		d.HandleWheel(0, -float64(max(1, d.viewportHeight-1)))
	case "pgdown":
		d.HandleWheel(0, float64(max(1, d.viewportHeight-1)))
	case "home":
		d.HandleWheel(0, -float64(d.contentHeight))
	case "end":
		d.HandleWheel(0, float64(d.contentHeight))
	}
	return false, nil
}

func (d *YesNo) HandleWheel(deltaX, deltaY float64) {
	d.scrollOffset = min(max(0, d.contentHeight-d.viewportHeight), max(0, d.scrollOffset+int(deltaY)))
	d.wheelActive = true
}

func (d *YesNo) answer(resp question.Answer) {
	d.lastResponse = resp
}

// Response returns the last response. Used by QuestionForm to
// collect answers from child components.
// Response returns the current answer, reflecting live selection
// state so that tabbing away preserves the choice.
func (d *YesNo) Response() question.Answer { return d.respond(!d.selectedNo) }

// GetRequest returns the underlying question request.
func (d *YesNo) GetRequest() question.Question { return d.Request }

// ShortHelp returns key bindings for the status bar help display.
func (d *YesNo) ShortHelp() []key.Binding {
	if d.activeNoteKey != "" && d.noteEditor.Focused() {
		return []key.Binding{d.keyClose, key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "save note"))}
	}
	return []key.Binding{d.keyLeftRight, d.keyEnter, d.keyYes, d.keyNo, d.keyNote, key.NewBinding(key.WithKeys("up", "down", "pgup", "pgdown"), key.WithHelp("↑/↓", "scroll"))}
}

func (d *YesNo) respond(yes bool) question.Answer {
	resp := question.Answer{
		QuestionID: d.Request.ID,
		Yes:        &yes,
	}
	if len(d.notes) > 0 {
		resp.Notes = make(map[string]string, len(d.notes))
		maps.Copy(resp.Notes, d.notes)
	}
	return resp
}

// Height returns the visual height at the default max width.
// Pure function — no render-time state.
func (d *YesNo) Height(width int) int {
	w := width
	if w <= 0 {
		w = d.lastWidth
	}
	if w <= 0 {
		w = choiceListMaxWidth
	}
	iconPrompt := questionIconPrompt(d.Styles, d.focused)
	h := sectionHeight(d.Request.Text, w-lipgloss.Width(iconPrompt)) // question
	h++                                                              // blank
	if d.Request.Description != "" {
		r := common.MarkdownRenderer(d.Styles, w)
		mu := common.LockMarkdownRenderer(r)
		mu.Lock()
		out, err := r.Render(d.Request.Description)
		mu.Unlock()
		if err == nil {
			out = strings.TrimSuffix(out, "\n")
			h += strings.Count(out, "\n") + 1
		} else {
			h += sectionHeight(d.Request.Description, w)
		}
		h++ // blank
	}
	_, buttons := d.buttons(w)
	h += lipgloss.Height(buttons)
	// Note height if present.
	if d.activeNoteKey != "" && d.noteEditor.Focused() {
		h++ // blank separator before note editor
		h += d.noteEditor.Height()
	} else if saved := d.notes["_question"]; saved != "" {
		h++
		h += lipgloss.Height(ansi.Hardwrap("> "+saved, max(1, w), true))
	}
	h++ // trailing blank for bottom padding
	return h
}

// Draw renders the yes/no question directly to screen.
// Returns the cursor position when the note editor is active, or nil.
func (d *YesNo) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	d.lastWidth = area.Dx()
	d.viewportRect = area
	d.viewportHeight = max(0, area.Dy())
	if area.Dx() <= 0 || d.viewportHeight == 0 {
		d.compositor = nil
		return nil
	}
	var buttonStart int
	var buttonOpts []common.ButtonOpts
	var buttons string
	var cursor *tea.Cursor
	build := func(width int) []string {
		iconPrompt := questionIconPrompt(d.Styles, d.focused)
		header := iconPrompt + d.Styles.Editor.QuestionUnselected.Render(ansi.Wrap(d.Request.Text, max(1, width-lipgloss.Width(iconPrompt)), ""))
		lines := strings.Split(ansi.Hardwrap(header, width, true), "\n")
		lines = append(lines, "")
		if d.Request.Description != "" {
			r := common.MarkdownRenderer(d.Styles, width)
			mu := common.LockMarkdownRenderer(r)
			mu.Lock()
			desc, err := r.Render(d.Request.Description)
			mu.Unlock()
			if err != nil {
				desc = d.Request.Description
			}
			lines = append(lines, strings.Split(ansi.Hardwrap(strings.TrimSuffix(desc, "\n"), width, true), "\n")...)
			lines = append(lines, "")
		}
		buttonStart = len(lines)
		buttonOpts, buttons = d.buttons(width)
		lines = append(lines, strings.Split(buttons, "\n")...)
		cursor = nil
		if d.activeNoteKey != "" && d.noteEditor.Focused() {
			lines = append(lines, "")
			d.noteEditor.SetWidth(max(1, width-4))
			if base := d.noteEditor.Cursor(); base != nil {
				copy := *base
				copy.X += 2
				copy.Y += len(lines)
				cursor = &copy
			}
			for index, line := range strings.Split(d.noteEditor.View(), "\n") {
				prefix := "  "
				if index == 0 {
					prefix = "> "
				}
				lines = append(lines, prefix+line)
			}
		} else if saved := d.notes["_question"]; saved != "" {
			lines = append(lines, "")
			lines = append(lines, strings.Split(ansi.Hardwrap("> "+d.Styles.Editor.QuestionNote.Render(saved), width, true), "\n")...)
		}
		return append(lines, "")
	}
	contentWidth := area.Dx()
	lines := build(contentWidth)
	overflow := len(lines) > d.viewportHeight
	if overflow && contentWidth > 1 {
		contentWidth--
		lines = build(contentWidth)
	}
	d.contentHeight = len(lines)
	maximum := max(0, d.contentHeight-d.viewportHeight)
	d.scrollOffset = min(maximum, max(0, d.scrollOffset))
	reveal := -1
	if d.revealSelection {
		reveal = buttonStart
		if d.selectedNo {
			reveal += lipgloss.Height(buttons) - 1
		}
		d.revealSelection = false
	}
	if cursor != nil && !d.wheelActive {
		reveal = cursor.Y
	}
	if reveal >= 0 {
		if reveal < d.scrollOffset {
			d.scrollOffset = reveal
		} else if reveal >= d.scrollOffset+d.viewportHeight {
			d.scrollOffset = reveal - d.viewportHeight + 1
		}
		d.scrollOffset = min(maximum, max(0, d.scrollOffset))
	}
	d.compositor = common.ButtonHitCompositorForView(d.Styles, buttonOpts, buttons, area.Min.X, area.Min.Y+buttonStart-d.scrollOffset)
	if image.Pt(d.hoverX, d.hoverY).In(area) {
		if hovered := common.HitButtonIndex(d.compositor, d.hoverX, d.hoverY); hovered >= 0 {
			buttonOpts[hovered].Hovered = true
			separator := " "
			if lipgloss.Height(buttons) > 1 {
				separator = "\n"
			}
			copy(lines[buttonStart:], strings.Split(common.ButtonGroup(d.Styles, buttonOpts, separator), "\n"))
		}
	}
	for row := range d.viewportHeight {
		index := d.scrollOffset + row
		if index >= len(lines) {
			break
		}
		y := area.Min.Y + row
		drawStyledText(scr, image.Rect(area.Min.X, y, area.Min.X+contentWidth, y+1), lines[index])
	}
	if overflow && area.Dx() > 1 {
		scrollbar := common.Scrollbar(d.Styles, d.viewportHeight, len(lines), d.viewportHeight, d.scrollOffset)
		uv.NewStyledString(scrollbar).Draw(scr, image.Rect(area.Max.X-1, area.Min.Y, area.Max.X, area.Max.Y))
	}
	if cursor != nil {
		cursor.Y -= d.scrollOffset
		if cursor.Y < 0 || cursor.Y >= d.viewportHeight || cursor.X >= contentWidth {
			return nil
		}
	}
	return cursor
}

func (d *YesNo) buttons(width int) ([]common.ButtonOpts, string) {
	opts := []common.ButtonOpts{
		{Text: "Yes", Selected: !d.selectedNo, Padding: 3, UnderlineIndex: 0},
		{Text: "No", Selected: d.selectedNo, Padding: 3, UnderlineIndex: 0},
	}
	buttons := common.ButtonGroup(d.Styles, opts, " ")
	if lipgloss.Width(buttons) > width {
		for index := range opts {
			opts[index].Padding = 1
		}
		buttons = common.ButtonGroup(d.Styles, opts, " ")
	}
	if lipgloss.Width(buttons) > width {
		buttons = common.ButtonGroup(d.Styles, opts, "\n")
	}
	return opts, buttons
}

// HeightChanged always returns false — Height is now pure.
func (d *YesNo) HeightChanged() bool { return false }

// SetFocused updates the icon style based on whether the editor
// area is focused.
func (d *YesNo) SetFocused(focused bool) { d.focused = focused }

// SetHover updates the hover position for button highlighting.
func (d *YesNo) SetHover(x, y int) { d.hoverX = x; d.hoverY = y }

// HandlePaste forwards paste events to the note editor textarea.
func (d *YesNo) HandlePaste(msg tea.PasteMsg) tea.Cmd {
	d.wheelActive = false
	return d.handlePaste(msg)
}

// HandleMouseClick checks if the click landed on a button and
// triggers the corresponding answer.
func (d *YesNo) HandleMouseClick(x, y int) (bool, bool) {
	if !image.Pt(x, y).In(d.viewportRect) {
		return false, false
	}
	switch common.HitButtonIndex(d.compositor, x, y) {
	case 0: // Yes
		d.selectedNo = false
		d.answer(d.respond(true))
		return true, true
	case 1: // No
		d.selectedNo = true
		d.answer(d.respond(false))
		return true, true
	}
	return false, false
}
