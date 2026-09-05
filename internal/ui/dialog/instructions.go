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
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent"
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
	replaced    bool
	kind        instrItemKind
	control     *providerregistry.RuntimeControlSurface
}

type instrItemKind int

const (
	instrHeader instrItemKind = iota
	instrMode                 // instruction mode radio
	instrNativeToggle
	instrSection
	instrProviderContextEdit
	instrMetadataValue
	instrPreview
	instrAction
)

// Instructions is a dialog for managing instruction sections and modes.
type Instructions struct {
	com                 *common.Common
	items               []instrItem
	cursor              int
	keyMap              instrKeyMap
	maxWidth            int
	projectInstrPath    string
	providerContextPath string
	providerID          string
	metadataInput       textinput.Model
	editingMetadata     bool
	viewport            viewport.Model
	followCursor        bool
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
	surfaces := com.Workspace.ProviderSurfaces()
	surface, _ := providerregistry.LookupSurface(surfaces, providerID)
	nativeLabel, nativeText := providerNativeInstructions(surfaces, providerID)
	nativeAvailable := strings.TrimSpace(nativeText) != ""
	toolingProfile := config.ToolingInstructionsCrux
	if nativeAvailable && surface.Instructions.SelectionDefault != "" {
		toolingProfile = surface.Instructions.SelectionDefault
	}
	if cfg.Providers != nil {
		if providerCfg, ok := cfg.Providers.Get(providerID); ok && providerCfg.ToolingInstructions != "" {
			toolingProfile = providerCfg.ToolingInstructions
		}
	}
	toolingHeader := "Tooling Profile"
	if providerID != "" {
		toolingHeader += " (" + providerID + ")"
	}

	var items []instrItem

	// Mode selector.
	items = append(items, instrItem{kind: instrHeader, label: "Instruction Mode"})
	for _, m := range []struct{ id, label string }{
		{"all", "Tooling + project context"},
		{"project", "Project context without tooling"},
		{"native", "Tooling without project context"},
	} {
		items = append(items, instrItem{
			kind:     instrMode,
			id:       m.id,
			label:    m.label,
			disabled: m.id != mode, // "disabled" here means "not selected"
		})
	}

	if nativeAvailable {
		label := "Use " + nativeLabel
		if surface.Instructions != nil && surface.Instructions.SelectionDefault == config.ToolingInstructionsNative {
			label += " (default)"
		}
		items = append(items, instrItem{kind: instrHeader, label: toolingHeader})
		items = append(items, instrItem{
			kind:     instrNativeToggle,
			id:       config.ToolingInstructionsNative,
			label:    label,
			disabled: toolingProfile != config.ToolingInstructionsNative,
		})
	}

	providerContextAvailable := validProviderInstructionsID(providerID)
	if providerContextAvailable {
		providerContextHeader := "Provider Context"
		if providerID != "" {
			providerContextHeader += " (" + providerID + ")"
		}
		items = append(items, instrItem{kind: instrHeader, label: providerContextHeader})
		items = append(items, instrItem{
			kind:  instrProviderContextEdit,
			id:    "edit-provider-context",
			label: "Edit ~/.ai-cli/instructions/" + providerID + ".txt",
		})
	}

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

	providerContextPath := ""
	if providerContextAvailable {
		providerContextPath = filepath.Join(home.Dir(), ".ai-cli", "instructions", providerID+".txt")
	}
	metadataInput := textinput.New()
	metadataInput.SetVirtualCursor(true)

	metadataInput.SetStyles(com.Styles.TextInput)

	d := &Instructions{
		com:                 com,
		items:               items,
		maxWidth:            72,
		projectInstrPath:    projPath,
		providerContextPath: providerContextPath,
		providerID:          providerID,
		metadataInput:       metadataInput,
		viewport:            viewport.New(),
		followCursor:        true,
		keyMap: instrKeyMap{
			Up:     key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
			Down:   key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
			Toggle: key.NewBinding(key.WithKeys("enter", " "), key.WithHelp("space/enter", "toggle")),
			Edit:   key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit file")),
			Close:  CloseKey,
		},
	}
	// Start cursor on first non-header item.
	d.updateReplacedSections()
	for i, item := range d.items {
		if d.selectable(item) {
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
			if d.items[d.cursor].kind == instrProviderContextEdit {
				return d.editProviderContextFile()
			}
			return d.editProjectFile()
		case key.Matches(msg, d.keyMap.Toggle):
			return d.toggle()
		}
	case common.CoalescedWheelMsg:
		d.followCursor = false
		d.viewport, _ = d.viewport.Update(tea.MouseWheelMsg(msg.Mouse))
	}
	return nil
}

func (d *Instructions) selectable(item instrItem) bool {
	return item.kind != instrHeader && !item.unavailable && !item.replaced
}

func (d *Instructions) moveCursor(dir int) {
	if len(d.items) == 0 {
		return
	}
	for step := 1; step <= len(d.items); step++ {
		index := (d.cursor + dir*step) % len(d.items)
		if index < 0 {
			index += len(d.items)
		}
		if d.selectable(d.items[index]) {
			d.cursor = index
			d.followCursor = true
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

	case instrNativeToggle:
		if item.unavailable || d.providerID == "" {
			return nil
		}
		profile := config.ToolingInstructionsNative
		if !item.disabled {
			profile = config.ToolingInstructionsCrux
		}
		if err := d.com.Workspace.SetConfigField(
			config.ScopeGlobal,
			"providers."+d.providerID+".tooling_instructions",
			profile,
		); err != nil {
			return ActionCmd{Cmd: util.ReportError(err)}
		}
		item.disabled = !item.disabled
		d.updateReplacedSections()
		return ActionInstructionsChanged{}

	case instrSection:
		if item.replaced {
			return nil
		}
		d.SetSectionDisabled(item.id, !item.disabled)
		return ActionInstructionsChanged{}

	case instrProviderContextEdit:
		return d.editProviderContextFile()

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
		return ActionCmd{Cmd: d.previewInstructionsCmd()}
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

func (d *Instructions) updateReplacedSections() {
	nativeSelected := false
	for _, item := range d.items {
		if item.kind == instrNativeToggle && !item.disabled {
			nativeSelected = true
			break
		}
	}
	for index := range d.items {
		if d.items[index].kind == instrSection {
			d.items[index].replaced = nativeSelected
		}
	}
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

func (d *Instructions) previewInstructionsCmd() tea.Cmd {
	workspace := d.com.Workspace
	return func() tea.Msg {
		snapshot, err := workspace.AgentInstructionSnapshot(context.Background())
		if err != nil {
			return util.ReportError(fmt.Errorf("failed to preview effective instructions: %w", err))()
		}
		return ActionPreviewInstructions{Sections: instructionPreviewSections(snapshot)}
	}
}

func instructionPreviewSections(snapshot agent.InstructionSnapshot) []InstructionPreviewSection {
	sections := make([]InstructionPreviewSection, 0, len(snapshot.Sections))
	for index, section := range snapshot.Sections {
		group := "Dynamic"
		if section.Stability == fantasy.InstructionStabilityStatic {
			group = "Static"
		}
		if snapshot.Policy == fantasy.InstructionPolicyAnthropic {
			group = "Uncached"
			if section.Stability == fantasy.InstructionStabilityStatic {
				group = "Cached"
			}
		}
		kind := sectionDisplayName(strings.ReplaceAll(string(section.Kind), "-", "_"))
		label := group + " · " + kind
		if section.CacheBoundary {
			label += " · boundary"
		}
		sections = append(sections, InstructionPreviewSection{
			ID:      fmt.Sprintf("%s-%d", section.Kind, index),
			Label:   label,
			Content: section.Text,
		})
	}
	return sections
}

func (d *Instructions) editProjectFile() Action {
	return d.editFile(d.projectInstrPath, "# Project Instructions\n\nAdd project-specific instructions here.\n")
}

func (d *Instructions) editProviderContextFile() Action {
	return d.editFile(d.providerContextPath, "")
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
	frameStyle := t.Dialog.View
	dialogWidth := max(min(d.maxWidth, area.Dx()), 1)
	innerWidth := max(dialogWidth-frameStyle.GetHorizontalFrameSize(), 1)

	rowCount := 0
	for _, item := range d.items {
		if item.kind == instrHeader {
			rowCount += 2
		} else {
			rowCount++
		}
	}
	title := t.Dialog.TitleText.Render("Instructions")
	dialogHeight := max(area.Dy()-2, 1)
	availableHeight := max(dialogHeight-frameStyle.GetVerticalFrameSize()-lipgloss.Height(title), 0)
	showHint := availableHeight >= 3
	if showHint {
		availableHeight -= 2
	}
	bodyHeight := min(rowCount, availableHeight)
	viewportWidth := innerWidth
	if rowCount > bodyHeight && bodyHeight > 0 {
		viewportWidth = max(viewportWidth-1, 1)
	}

	rows := make([]string, 0, rowCount)
	itemRows := make(map[int]int, len(d.items))
	for index, item := range d.items {
		switch item.kind {
		case instrHeader:
			rows = append(rows, "")
			label := ansi.Truncate("  "+item.label, viewportWidth, "…")
			rows = append(rows, t.Dialog.PrimaryText.Bold(true).Render(label))
		case instrMode:
			itemRows[index] = len(rows)
			radio := "○"
			if !item.disabled {
				radio = "●"
			}
			label := ansi.Truncate(fmt.Sprintf("  %s %s", radio, item.label), viewportWidth, "…")
			if item.unavailable {
				rows = append(rows, t.Dialog.SecondaryText.Render(label))
			} else {
				rows = append(rows, d.styledRow(t, index, label))
			}
		case instrNativeToggle, instrSection:
			itemRows[index] = len(rows)
			check := "✓"
			if item.disabled {
				check = " "
			}
			label := fmt.Sprintf("  [%s] %s", check, item.label)
			if item.replaced {
				label = "  [-] " + item.label + " (replaced by native)"
			}
			label = ansi.Truncate(label, viewportWidth, "…")
			if item.unavailable || item.replaced {
				rows = append(rows, t.Dialog.SecondaryText.Render(label))
			} else {
				rows = append(rows, d.styledRow(t, index, label))
			}
		case instrMetadataValue:
			itemRows[index] = len(rows)
			value := item.value
			if value == "" {
				value = "unset"
			}
			label := fmt.Sprintf("  ▸ %s: %s", item.label, value)
			if d.editingMetadata && index == d.cursor {
				label = "  ▸ " + item.label + ": " + d.metadataInput.View()
			}
			label = ansi.Truncate(label, viewportWidth, "…")
			if item.unavailable {
				rows = append(rows, t.Dialog.SecondaryText.Render(label))
			} else {
				rows = append(rows, d.styledRow(t, index, label))
			}
		case instrProviderContextEdit, instrPreview, instrAction:
			itemRows[index] = len(rows)
			label := ansi.Truncate("  ▸ "+item.label, viewportWidth, "…")
			if item.unavailable {
				rows = append(rows, t.Dialog.SecondaryText.Render(label))
			} else {
				rows = append(rows, d.styledRow(t, index, label))
			}
		}
	}

	var body string
	if bodyHeight > 0 {
		d.viewport.SetWidth(viewportWidth)
		d.viewport.SetHeight(bodyHeight)
		d.viewport.SetContentLines(rows)
		if d.followCursor {
			if row, ok := itemRows[d.cursor]; ok {
				offset := d.viewport.YOffset()
				switch {
				case row < offset:
					d.viewport.SetYOffset(row)
				case row >= offset+bodyHeight:
					d.viewport.SetYOffset(row - bodyHeight + 1)
				}
			}
			d.followCursor = false
		}
		body = d.viewport.View()
		if rowCount > bodyHeight {
			body = joinScrollbar(t, body, bodyHeight, rowCount, bodyHeight, d.viewport.YOffset())
		}
	}

	parts := []string{title}
	if body != "" {
		parts = append(parts, body)
	}
	if showHint {
		hint := ansi.Truncate("  space: select · e: edit file · esc: close", innerWidth, "…")
		parts = append(parts, "", t.Dialog.SecondaryText.Render(hint))
	}
	content := lipgloss.JoinVertical(lipgloss.Left, parts...)
	DrawCenter(scr, area, frameStyle.Width(dialogWidth).Render(content))
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

func validProviderInstructionsID(provider string) bool {
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
