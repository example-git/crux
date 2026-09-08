package dialog

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/common"
)

const SummarizationID = "summarization"

type Summarization struct {
	com          *common.Common
	cursor       int
	editing      bool
	input        textinput.Model
	disabled     bool
	contextCap   int64
	maxTokens    int64
	fastMode     bool
	compactionV2 bool
	status       string
}

func NewSummarization(com *common.Common) *Summarization {
	opts := com.Config().Options
	d := &Summarization{com: com, disabled: opts.DisableAutoSummarize, contextCap: opts.SummarizationContextCap, maxTokens: opts.SummarizationMaxTokens, fastMode: opts.SummarizationFastMode, compactionV2: opts.CodexCompactionV2}
	d.input = textinput.New()
	d.input.SetVirtualCursor(true)
	d.input.SetStyles(com.Styles.TextInput)
	d.input.CharLimit = 19
	return d
}

func (*Summarization) ID() string { return SummarizationID }

func (d *Summarization) HandleMsg(msg tea.Msg) Action {
	if press, ok := msg.(tea.KeyPressMsg); ok {
		if key.Matches(press, CloseKey) {
			if d.editing {
				d.editing = false
				d.input.Blur()
				return nil
			}
			return ActionClose{}
		}
		if d.editing {
			if press.String() == "enter" {
				value, err := strconv.ParseInt(strings.TrimSpace(d.input.Value()), 10, 64)
				if err != nil || value < 0 {
					d.status = "Enter a non-negative integer; 0 uses the model default."
					return nil
				}
				field := "options.summarization_context_cap"
				if d.cursor == 2 {
					field = "options.summarization_max_tokens"
				}
				if err := d.com.Workspace.SetConfigField(config.ScopeWorkspace, field, value); err != nil {
					d.status = err.Error()
					return nil
				}
				if d.cursor == 1 {
					d.contextCap = value
				} else {
					d.maxTokens = value
				}
				d.editing = false
				d.input.Blur()
				d.status = "Saved for this project. Applies to newly admitted turns."
				return nil
			}
		} else {
			switch press.String() {
			case "up", "shift+tab", "k":
				d.cursor = (d.cursor + 4) % 5
				return nil
			case "down", "tab", "j":
				d.cursor = (d.cursor + 1) % 5
				return nil
			case "enter", "space":
				if d.cursor == 4 {
					if err := d.com.Workspace.SetConfigField(config.ScopeWorkspace, "options.codex_compaction_v2", !d.compactionV2); err != nil {
						d.status = err.Error()
						return nil
					}
					d.compactionV2 = !d.compactionV2
					d.status = "Saved for this project. Applies to newly admitted turns."
					return nil
				}
				if d.cursor == 3 {
					if err := d.com.Workspace.SetConfigField(config.ScopeWorkspace, "options.summarization_fast_mode", !d.fastMode); err != nil {
						d.status = err.Error()
						return nil
					}
					d.fastMode = !d.fastMode
					d.status = "Saved for this project. Applies to newly admitted turns."
					return nil
				}
				if d.cursor == 0 {
					if err := d.com.Workspace.SetConfigField(config.ScopeWorkspace, "options.disable_auto_summarize", !d.disabled); err != nil {
						d.status = err.Error()
						return nil
					}
					d.disabled = !d.disabled
					d.status = "Saved for this project. Applies to newly admitted turns."
					return nil
				}
				value := d.contextCap
				if d.cursor == 2 {
					value = d.maxTokens
				}
				d.input.SetValue(strconv.FormatInt(value, 10))
				d.input.CursorEnd()
				d.editing = true
				return ActionCmd{Cmd: d.input.Focus()}
			}
		}
	}
	if d.editing {
		var cmd tea.Cmd
		d.input, cmd = d.input.Update(msg)
		return ActionCmd{Cmd: cmd}
	}
	return nil
}

func (d *Summarization) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(1, min(70, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	rc := NewRenderContext(t, width)
	rc.Title = "Summarization"
	innerWidth := max(1, width-t.Dialog.View.GetHorizontalFrameSize())
	boolean := func(value bool) string {
		if value {
			return "On"
		}
		return "Off"
	}
	number := func(value int64) string {
		if value == 0 {
			return "Model default"
		}
		return strconv.FormatInt(value, 10)
	}
	rows := []string{
		"Automatic summarization: " + boolean(!d.disabled),
		"Context window cap: " + number(d.contextCap),
		"Summary output tokens: " + number(d.maxTokens),
		"Codex fast mode: " + boolean(d.fastMode),
		"Codex compaction v2 (experimental): " + boolean(d.compactionV2),
	}
	for i, row := range rows {
		style := t.Dialog.NormalItem
		if d.cursor == i {
			style = t.Dialog.SelectedItem
		}
		rc.AddPart(style.Width(innerWidth).Render(ansi.Truncate(row, max(1, innerWidth-style.GetHorizontalFrameSize()), "…")))
		if d.cursor == i && d.editing {
			d.input.SetWidth(max(1, innerWidth-4))
			rc.AddPart(t.Dialog.NormalItem.Render(d.input.View()))
		}
	}
	explanations := []string{
		"Automatically summarize when the context window fills.",
		"0 uses the model default. The cap only lowers the window; compaction leaves the normal reserve.",
		"0 uses the model default. Applies to readable summaries, not encrypted compaction.",
		"Use priority service for Codex compaction and summaries only.",
		"Experimental: encrypted compaction without a readable summary. Normally Codex uses one readable summary request.",
	}
	rc.AddPart("\n" + t.Dialog.NormalItem.Foreground(t.Tool.SummaryMeta.GetForeground()).Width(innerWidth).Render(ansi.Wrap(explanations[d.cursor], max(1, innerWidth-t.Dialog.NormalItem.GetHorizontalFrameSize()), "")))
	if d.status != "" {
		rc.AddPart(t.Dialog.NormalItem.Width(innerWidth).Render(ansi.Wrap(d.status, max(1, innerWidth-t.Dialog.NormalItem.GetHorizontalFrameSize()), "")))
	}
	h := help.New()
	h.Styles = t.DialogHelpStyles()
	bindings := []key.Binding{
		key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "close")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "edit/toggle")),
		key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑/↓", "choose")),
	}
	if d.editing {
		bindings[0] = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel"))
		bindings[1] = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "save"))
		bindings = bindings[:2]
	}
	rc.Help = t.Dialog.HelpView.Render(ShortHelpLine(&h, bindings, max(0, innerWidth-t.Dialog.HelpView.GetHorizontalFrameSize())))
	DrawCenterCursor(scr, area, rc.Render(), nil)
	return nil
}
