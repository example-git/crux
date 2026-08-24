package dialog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/example-git/crux/internal/agent/prompt"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/home"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/tidwall/gjson"
)

// InstructionsID is the identifier for the instructions dialog.
const InstructionsID = "instructions"

// ActionInstructionsChanged signals that instruction config was modified.
type ActionInstructionsChanged struct{}

// ActionEditProjectInstructions opens the project instructions file in $EDITOR.
type ActionEditProjectInstructions struct {
	Path string
}

type ActionPreviewInstructions struct {
	Sections []InstructionPreviewSection
}

type ActionPreviewInstructionSectionToggled struct {
	ID       string
	Disabled bool
	Cmd      tea.Cmd
}

type instrItem struct {
	id          string
	label       string
	value       string
	disabled    bool
	unavailable bool
	kind        instrItemKind
	control     *providerregistry.RuntimeControlSurface
}

type instrItemKind int

const (
	instrHeader instrItemKind = iota
	instrMode                 // instruction mode radio
	instrToolingProfile
	instrSection
	instrOverrideToggle
	instrOverrideEdit
	instrMetadataValue
	instrPreview
	instrAction
)

// Instructions is a dialog for managing instruction sections and modes.
type Instructions struct {
	com              *common.Common
	items            []instrItem
	cursor           int
	keyMap           instrKeyMap
	maxWidth         int
	projectInstrPath string
	overridePath     string
	providerID       string
	metadataInput    textinput.Model
	editingMetadata  bool
}

type instrKeyMap struct {
	Up     key.Binding
	Down   key.Binding
	Toggle key.Binding
	Edit   key.Binding
	Close  key.Binding
}

var _ Dialog = (*Instructions)(nil)

// NewInstructions builds the instructions dialog.
func NewInstructions(com *common.Common) *Instructions {
	cfg := com.Config()
	mode := cfg.Options.InstructionMode
	if mode == "" {
		mode = "all"
	}
	disabledSet := make(map[string]bool, len(cfg.Options.DisabledInstructionSections))
	for _, id := range cfg.Options.DisabledInstructionSections {
		disabledSet[id] = true
	}
	selectedModel := cfg.Models[config.SelectedModelTypeLarge]
	providerID := selectedModel.Provider
	toolingProfile := config.ToolingInstructionsCrux
	if cfg.Providers != nil {
		if providerCfg, ok := cfg.Providers.Get(providerID); ok && providerCfg.ToolingInstructions != "" {
			toolingProfile = providerCfg.ToolingInstructions
		}
	}
	surfaces := com.Workspace.ProviderSurfaces()
	surface, registered := providerregistry.LookupSurface(surfaces, providerID)
	nativeAvailable := registered && surface.Instructions != nil
	toolingHeader := "Tooling Profile"
	if providerID != "" {
		toolingHeader += " (" + providerID + ")"
	}

	var items []instrItem

	// Mode selector.
	items = append(items, instrItem{kind: instrHeader, label: "Instruction Mode"})
	for _, m := range []struct{ id, label string }{
		{"all", "All (native + project)"},
		{"project", "Project instructions only"},
		{"native", "Native instructions only"},
	} {
		items = append(items, instrItem{
			kind:     instrMode,
			id:       m.id,
			label:    m.label,
			disabled: m.id != mode, // "disabled" here means "not selected"
		})
	}

	items = append(items, instrItem{kind: instrHeader, label: toolingHeader})
	items = append(items,
		instrItem{
			kind:     instrToolingProfile,
			id:       config.ToolingInstructionsCrux,
			label:    "Crux instructions",
			disabled: toolingProfile != config.ToolingInstructionsCrux,
		},
		instrItem{
			kind:        instrToolingProfile,
			id:          config.ToolingInstructionsNative,
			label:       "Provider native instructions",
			disabled:    toolingProfile != config.ToolingInstructionsNative,
			unavailable: !nativeAvailable,
		},
	)

	overrideAvailable := validSystemPromptOverrideProvider(providerID)
	overrideHeader := "System Prompt Override"
	if providerID != "" {
		overrideHeader += " (" + providerID + ")"
	}
	items = append(items, instrItem{kind: instrHeader, label: overrideHeader})
	items = append(items,
		instrItem{
			kind:        instrOverrideToggle,
			id:          "system-prompt-override",
			label:       "Use provider override",
			disabled:    !cfg.Options.SystemPromptOverride,
			unavailable: !overrideAvailable,
		},
		instrItem{
			kind:        instrOverrideEdit,
			id:          "edit-system-prompt-override",
			label:       "Edit ~/.ai-cli/instructions/" + providerID + ".txt",
			unavailable: !overrideAvailable,
		},
	)

	runtimeHeader := "Provider Runtime Controls"
	if surface.Name != "" {
		runtimeHeader = surface.Name + " Runtime Controls"
	}
	items = append(items, instrItem{kind: instrHeader, label: runtimeHeader})
	for i := range surface.RuntimeControls {
		control := surface.RuntimeControls[i]
		label := control.Label
		if len(control.Values) > 0 {
			label += " (" + strings.Join(control.Values, "/") + ")"
		}
		items = append(items, instrItem{
			kind:        instrMetadataValue,
			id:          control.ID,
			label:       label,
			value:       runtimeControlValue(cfg, control.ID),
			unavailable: !control.Available,
			control:     &control,
		})
	}

	items = append(items, instrItem{kind: instrHeader, label: "Crux Sections"})
	for _, s := range prompt.AllSections() {
		items = append(items, instrItem{
			kind:     instrSection,
			id:       s.ID,
			label:    sectionDisplayName(s.ID),
			disabled: disabledSet[s.ID],
		})
	}

	// Edit action.
	projPath := config.AiCliProjectInstructionsPath(com.Workspace.WorkingDir())
	items = append(items, instrItem{kind: instrHeader, label: "Project Instructions"})
	items = append(items, instrItem{
		kind:  instrPreview,
		id:    "preview",
		label: "Preview active instructions",
	})
	items = append(items, instrItem{
		kind:  instrAction,
		id:    "edit",
		label: "Edit project instructions in $EDITOR",
	})

	overridePath := ""
	if overrideAvailable {
		overridePath = filepath.Join(home.Dir(), ".ai-cli", "instructions", providerID+".txt")
	}
	metadataInput := textinput.New()
	metadataInput.SetVirtualCursor(true)

	metadataInput.SetStyles(com.Styles.TextInput)

	d := &Instructions{
		com:              com,
		items:            items,
		maxWidth:         72,
		projectInstrPath: projPath,
		overridePath:     overridePath,
		providerID:       providerID,
		metadataInput:    metadataInput,
		keyMap: instrKeyMap{
			Up:     key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
			Down:   key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
			Toggle: key.NewBinding(key.WithKeys("enter", " "), key.WithHelp("space/enter", "toggle")),
			Edit:   key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit file")),
			Close:  CloseKey,
		},
	}
	// Start cursor on first non-header item.
	for i, item := range d.items {
		if item.kind != instrHeader {
			d.cursor = i
			break
		}
	}
	return d
}

func (*Instructions) ID() string { return InstructionsID }

func (d *Instructions) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if d.editingMetadata {
			switch {
			case key.Matches(msg, d.keyMap.Close):
				d.editingMetadata = false
				d.metadataInput.Blur()
				return nil
			case msg.Code == tea.KeyEnter:
				return d.saveMetadataValue()
			default:
				var cmd tea.Cmd
				d.metadataInput, cmd = d.metadataInput.Update(msg)
				return ActionCmd{Cmd: cmd}
			}
		}
		switch {
		case key.Matches(msg, d.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, d.keyMap.Up):
			d.moveCursor(-1)
		case key.Matches(msg, d.keyMap.Down):
			d.moveCursor(1)
		case key.Matches(msg, d.keyMap.Edit):
			if d.items[d.cursor].kind == instrOverrideEdit {
				return d.editOverrideFile()
			}
			return d.editProjectFile()
		case key.Matches(msg, d.keyMap.Toggle):
			return d.toggle()
		}
	}
	return nil
}

func (d *Instructions) moveCursor(dir int) {
	for {
		d.cursor += dir
		if d.cursor < 0 {
			d.cursor = 0
			return
		}
		if d.cursor >= len(d.items) {
			d.cursor = len(d.items) - 1
			return
		}
		if d.items[d.cursor].kind != instrHeader && !d.items[d.cursor].unavailable {
			return
		}
	}
}

func (d *Instructions) toggle() Action {
	if d.cursor >= len(d.items) {
		return nil
	}
	item := &d.items[d.cursor]

	switch item.kind {
	case instrMode:
		// Radio: select this mode, deselect others.
		for i := range d.items {
			if d.items[i].kind == instrMode {
				d.items[i].disabled = d.items[i].id != item.id
			}
		}
		_ = d.com.Workspace.SetConfigField(config.ScopeGlobal, "options.instruction_mode", item.id)
		return ActionInstructionsChanged{}

	case instrToolingProfile:
		if item.unavailable || d.providerID == "" {
			return nil
		}
		if err := d.com.Workspace.SetConfigField(
			config.ScopeGlobal,
			"providers."+d.providerID+".tooling_instructions",
			item.id,
		); err != nil {
			return ActionCmd{Cmd: util.ReportError(err)}
		}
		for i := range d.items {
			if d.items[i].kind == instrToolingProfile {
				d.items[i].disabled = d.items[i].id != item.id
			}
		}
		return ActionInstructionsChanged{}

	case instrSection:
		d.SetSectionDisabled(item.id, !item.disabled)
		return ActionInstructionsChanged{}

	case instrOverrideToggle:
		if item.unavailable {
			return nil
		}
		enabled := item.disabled
		if err := d.com.Workspace.SetConfigField(config.ScopeGlobal, "options.system_prompt_override", enabled); err != nil {
			return ActionCmd{Cmd: util.ReportError(err)}
		}
		item.disabled = !enabled
		return ActionInstructionsChanged{}

	case instrOverrideEdit:
		return d.editOverrideFile()

	case instrMetadataValue:
		if item.unavailable {
			return nil
		}
		d.metadataInput.SetValue(item.value)
		d.metadataInput.CursorEnd()
		d.metadataInput.Focus()
		d.editingMetadata = true
		return nil

	case instrPreview:
		return ActionPreviewInstructions{Sections: d.activeInstructionSections()}
	case instrAction:
		return d.editProjectFile()
	}
	return nil
}

func (d *Instructions) saveMetadataValue() Action {
	item := &d.items[d.cursor]
	value := strings.TrimSpace(d.metadataInput.Value())
	if value == "" {
		if err := d.com.Workspace.RemoveConfigField(config.ScopeGlobal, item.id); err != nil {
			return ActionCmd{Cmd: util.ReportError(err)}
		}
		item.value = ""
	} else {
		parsed, ok := parseRuntimeControlValue(item.control, value)
		if !ok {
			return ActionCmd{Cmd: util.ReportError(fmt.Errorf("invalid %s value %q", item.label, value))}
		}
		if err := d.com.Workspace.SetConfigField(config.ScopeGlobal, item.id, parsed); err != nil {
			return ActionCmd{Cmd: util.ReportError(err)}
		}
		item.value = value
	}
	d.editingMetadata = false
	d.metadataInput.Blur()
	return ActionInstructionsChanged{}
}

func (d *Instructions) SetSectionDisabled(id string, disabled bool) {
	for index := range d.items {
		if d.items[index].kind == instrSection && d.items[index].id == id {
			d.items[index].disabled = disabled
			break
		}
	}
	var disabledSections []string
	for _, item := range d.items {
		if item.kind == instrSection && item.disabled {
			disabledSections = append(disabledSections, item.id)
		}
	}
	_ = d.com.Workspace.SetConfigField(config.ScopeGlobal, "options.disabled_instruction_sections", disabledSections)
}

func (d *Instructions) activeInstructions() string {
	sections := d.activeInstructionSections()
	var parts []string
	for _, section := range sections {
		if !section.Disabled {
			parts = append(parts, section.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

func (d *Instructions) activeInstructionSections() []InstructionPreviewSection {
	for _, item := range d.items {
		if item.kind == instrOverrideToggle && !item.disabled {
			content, err := os.ReadFile(d.overridePath)
			if err == nil {
				return []InstructionPreviewSection{{Label: "Provider system prompt override", Content: string(content)}}
			}
			break
		}
	}

	mode := "all"
	toolingProfile := config.ToolingInstructionsCrux
	disabled := make(map[string]bool)
	labels := make(map[string]string)
	for _, item := range d.items {
		switch item.kind {
		case instrMode:
			if !item.disabled {
				mode = item.id
			}
		case instrToolingProfile:
			if !item.disabled {
				toolingProfile = item.id
			}
		case instrSection:
			disabled[item.id] = item.disabled
			labels[item.id] = item.label
		}
	}

	var sections []InstructionPreviewSection
	if mode != "project" {
		if toolingProfile == config.ToolingInstructionsNative {
			if label, content := providerNativeInstructions(d.com.Workspace.ProviderSurfaces(), d.providerID); content != "" {
				sections = append(sections, InstructionPreviewSection{
					Label:   label,
					Content: content,
				})
			}
		} else {
			for _, section := range prompt.AllSections() {
				label := labels[section.ID]
				if label == "" {
					label = sectionDisplayName(section.ID)
				}
				sections = append(sections, InstructionPreviewSection{
					ID:         section.ID,
					Label:      label,
					Content:    strings.TrimSpace(section.Content),
					Disabled:   disabled[section.ID],
					Toggleable: true,
				})
			}
		}
	}
	if mode != "native" {
		project, err := os.ReadFile(d.projectInstrPath)
		if err == nil && strings.TrimSpace(string(project)) != "" {
			sections = append(sections, InstructionPreviewSection{
				Label:   "Project instructions",
				Content: string(project),
			})
		}
	}
	return sections
}

func (d *Instructions) editProjectFile() Action {
	return d.editFile(d.projectInstrPath, "# Project Instructions\n\nAdd project-specific instructions here.\n")
}

func (d *Instructions) editOverrideFile() Action {
	return d.editFile(d.overridePath, d.activeInstructions())
}

func (d *Instructions) editFile(path, initialContent string) Action {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ActionCmd{Cmd: util.ReportError(err)}
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(initialContent), 0o600); err != nil {
			return ActionCmd{Cmd: util.ReportError(err)}
		}
	} else if err != nil {
		return ActionCmd{Cmd: util.ReportError(err)}
	}

	editorName := os.Getenv("EDITOR")
	if editorName == "" {
		editorName = "vi"
	}
	cmd := exec.CommandContext(context.Background(), editorName, path)
	return ActionCmd{Cmd: tea.ExecProcess(cmd, func(err error) tea.Msg {
		return ActionInstructionsChanged{}
	})}
}

func (d *Instructions) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	titleStyle := t.Dialog.TitleText

	var rows []string
	for i, item := range d.items {
		switch item.kind {
		case instrHeader:
			rows = append(rows, "")
			rows = append(rows, t.Dialog.PrimaryText.Bold(true).Render("  "+item.label))
		case instrMode, instrToolingProfile:
			radio := "○"
			if !item.disabled { // not-disabled = selected
				radio = "●"
			}
			label := fmt.Sprintf("  %s %s", radio, item.label)
			if item.unavailable {
				rows = append(rows, t.Dialog.SecondaryText.Render(label))
			} else {
				rows = append(rows, d.styledRow(t, i, label))
			}
		case instrSection, instrOverrideToggle:
			check := "✓"
			if item.disabled {
				check = " "
			}
			label := fmt.Sprintf("  [%s] %s", check, item.label)
			if item.unavailable {
				rows = append(rows, t.Dialog.SecondaryText.Render(label))
			} else {
				rows = append(rows, d.styledRow(t, i, label))
			}
		case instrMetadataValue:
			value := item.value
			if value == "" {
				value = "unset"
			}
			label := fmt.Sprintf("  ▸ %s: %s", item.label, value)
			if d.editingMetadata && i == d.cursor {
				label = "  ▸ " + item.label + ": " + d.metadataInput.View()
			}
			if item.unavailable {
				rows = append(rows, t.Dialog.SecondaryText.Render(label+" (unavailable for this model)"))
			} else {
				rows = append(rows, d.styledRow(t, i, label))
			}
		case instrOverrideEdit, instrPreview, instrAction:
			label := "  ▸ " + item.label
			if item.unavailable {
				rows = append(rows, t.Dialog.SecondaryText.Render(label))
			} else {
				rows = append(rows, d.styledRow(t, i, label))
			}
		}
	}

	hint := t.Dialog.SecondaryText.Render("  space: select · e: edit file · esc: close")
	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	content := lipgloss.JoinVertical(
		lipgloss.Left,
		titleStyle.Render("Instructions"),
		body,
		"",
		hint,
	)

	frameStyle := t.Dialog.View
	view := frameStyle.MaxWidth(d.maxWidth).Render(content)
	DrawCenter(scr, area, view)
	return nil
}

func (d *Instructions) styledRow(t *styles.Styles, idx int, label string) string {
	if idx == d.cursor {
		return t.Dialog.SelectedItem.Render(label)
	}
	return t.Dialog.NormalItem.Render(label)
}

func (*Instructions) ShortHelp() []key.Binding  { return nil }
func (*Instructions) FullHelp() [][]key.Binding { return nil }

func providerNativeInstructions(surfaces []providerregistry.Surface, providerID string) (string, string) {
	surface, ok := providerregistry.LookupSurface(surfaces, providerID)
	if !ok || surface.Instructions == nil {
		return "", ""
	}
	text, ok := surface.Instructions.Profiles[surface.Instructions.Default]
	if !ok {
		return "", ""
	}
	return surface.Name + " native tooling instructions", text
}

func validSystemPromptOverrideProvider(provider string) bool {
	return provider != "" && provider != "." && provider != ".." && !strings.ContainsAny(provider, `/\\`)
}

func runtimeControlValue(cfg *config.Config, path string) string {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	value := gjson.GetBytes(encoded, path)
	if !value.Exists() {
		return ""
	}
	return value.String()
}

func parseRuntimeControlValue(control *providerregistry.RuntimeControlSurface, value string) (any, bool) {
	if control == nil || !control.Available {
		return nil, false
	}
	switch control.Type {
	case "enum":
		return value, slices.Contains(control.Values, value)
	case "string":
		return value, true
	case "boolean":
		parsed, err := strconv.ParseBool(value)
		return parsed, err == nil
	case "integer":
		parsed, err := strconv.ParseInt(value, 10, 64)
		return parsed, err == nil
	case "number":
		parsed, err := strconv.ParseFloat(value, 64)
		return parsed, err == nil
	default:
		return nil, false
	}
}

func sectionDisplayName(id string) string {
	name := strings.ReplaceAll(id, "_", " ")
	if len(name) > 0 {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	return name
}
