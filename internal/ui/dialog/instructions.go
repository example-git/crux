package dialog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/bubbles/key"
	"github.com/example-git/crux/foundation/bubbles/textinput"
	"github.com/example-git/crux/foundation/bubbles/viewport"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/agent/prompt"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/home"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
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
	id             string
	label          string
	value          string
	disabled       bool
	unavailable    bool
	replaced       bool
	kind           instrItemKind
	control        *providerregistry.RuntimeControlSurface
	controlState   *config.RuntimeControlState
	controlLoading bool
	controlError   string
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
	providerModel       string
	providerOwner       providerregistry.RegistrationOwner
	providerOwnerSet    bool
	operationGeneration uint64
	operationPending    bool
	controlsGeneration  uint64
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
	Reset  key.Binding
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
	if len(surface.RuntimeControls) > 0 {
		items = append(items, instrItem{kind: instrHeader, label: runtimeHeader})
	}
	for i := range surface.RuntimeControls {
		control := surface.RuntimeControls[i]
		label := control.Label
		if len(control.Values) > 0 {
			label += " (" + strings.Join(control.Values, "/") + ")"
		}
		items = append(items, instrItem{
			kind:         instrMetadataValue,
			id:           control.ID,
			label:        label,
			controlError: control.Diagnostic,
			unavailable:  !control.Available,
			control:      &control,
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
		providerModel:       selectedModel.Model,
		metadataInput:       metadataInput,
		viewport:            viewport.New(),
		followCursor:        true,
		keyMap: instrKeyMap{
			Up:     key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
			Down:   key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
			Toggle: key.NewBinding(key.WithKeys("enter", " "), key.WithHelp("space/enter", "toggle")),
			Edit:   key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit file")),
			Reset:  key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "reset control")),
			Close:  CloseKey,
		},
	}
	if surface.Owner != nil {
		d.providerOwner, d.providerOwnerSet = *surface.Owner, true
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
			case key.Matches(msg, d.keyMap.Reset):
				return d.removeMetadataValue()
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
		case key.Matches(msg, d.keyMap.Reset):
			if d.items[d.cursor].kind == instrMetadataValue {
				return d.removeMetadataValue()
			}
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
		return d.mutate(instructionMutation{kind: instrMode, id: item.id, value: item.id})
	case instrNativeToggle:
		if item.unavailable || d.providerID == "" {
			return nil
		}
		profile := config.ToolingInstructionsNative
		if !item.disabled {
			profile = config.ToolingInstructionsCrux
		}
		return d.mutate(instructionMutation{kind: instrNativeToggle, id: item.id, value: profile, disabled: !item.disabled})
	case instrSection:
		if item.replaced {
			return nil
		}
		return d.SetSectionDisabled(item.id, !item.disabled)

	case instrProviderContextEdit:
		return d.editProviderContextFile()

	case instrMetadataValue:
		if err := d.checkControlItem(item); err != nil {
			return ActionCmd{Cmd: util.ReportError(err)}
		}
		value := item.controlState.Effective
		d.metadataInput.Placeholder = "Effective value; saved scope unknown"
		if item.controlState.ScopedKnown {
			value = item.controlState.Scoped
			d.metadataInput.Placeholder = "Inherited: " + runtimeControlDisplayValue(item.controlState.Effective)
		}
		if item.controlState.RuntimeDependent {
			d.metadataInput.Placeholder = "Configured fallback; saved scope unknown"
			if item.controlState.ScopedKnown {
				d.metadataInput.Placeholder = "Configured fallback: " + runtimeControlDisplayValue(item.controlState.Effective)
			}
		}
		d.metadataInput.SetValue(runtimeControlInputValue(value))
		d.metadataInput.CursorEnd()
		d.metadataInput.Focus()
		d.editingMetadata = true
		d.followCursor = true
		return nil

	case instrPreview:
		return ActionCmd{Cmd: d.previewInstructionsCmd()}
	case instrAction:
		return d.editProjectFile()
	}
	return nil
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

func (d *Instructions) SetSectionDisabled(id string, disabled bool) Action {
	disabledSections := []string{}
	for _, item := range d.items {
		if item.kind == instrSection && ((item.id == id && disabled) || (item.id != id && item.disabled)) {
			disabledSections = append(disabledSections, item.id)
		}
	}
	return d.mutate(instructionMutation{kind: instrSection, id: id, value: disabledSections, disabled: disabled})
}

// InstructionOperation binds asynchronous work to the dialog and exact provider
// selection that started it. Its payload is immutable once a command starts.
type InstructionOperation struct {
	Dialog              *Instructions
	generation          uint64
	providerID, modelID string
	owner               providerregistry.RegistrationOwner
	ownerSet            bool
	mutation            instructionMutation
}

type instructionMutation struct {
	kind             instrItemKind
	id               string
	value            any
	controlTarget    config.RuntimeControlTarget
	disabled, remove bool
}

type ActionInstructionMutationCompleted struct {
	Operation    InstructionOperation
	ControlState *config.RuntimeControlState
	Err          error
}

type ActionInstructionEditorPrepared struct {
	Operation InstructionOperation
	Cmd       tea.Cmd
	Err       error
}

type ActionInstructionEditorExited struct {
	Operation InstructionOperation
	Err       error
}

func (op InstructionOperation) validateSelection(ws workspace.Workspace) error {
	cfg := ws.Config()
	if cfg == nil {
		return fmt.Errorf("configuration not found")
	}
	selected := cfg.Models[config.SelectedModelTypeLarge]
	surface, _ := providerregistry.LookupSurface(ws.ProviderSurfaces(), selected.Provider)
	if selected.Provider != op.providerID || selected.Model != op.modelID ||
		(surface.Owner != nil) != op.ownerSet || (surface.Owner != nil && *surface.Owner != op.owner) {
		return fmt.Errorf("instruction provider selection changed; reopen the instructions dialog")
	}
	if op.mutation.kind == instrMetadataValue {
		return validateRuntimeControlTarget(ws, op.mutation.controlTarget)
	}
	return nil
}

// CheckOperation runs only on the UI thread, including when an editor returns.
func (d *Instructions) CheckOperation(op InstructionOperation) error {
	if op.Dialog != d || !d.operationPending || op.generation != d.operationGeneration {
		return fmt.Errorf("instruction operation is no longer current")
	}
	return op.validateSelection(d.com.Workspace)
}

func (d *Instructions) beginOperation(mutation instructionMutation) (InstructionOperation, error) {
	if d.operationPending {
		return InstructionOperation{}, fmt.Errorf("an instruction change is still pending")
	}
	op := InstructionOperation{
		Dialog: d, generation: d.operationGeneration + 1,
		providerID: d.providerID, modelID: d.providerModel, owner: d.providerOwner,
		ownerSet: d.providerOwnerSet, mutation: mutation,
	}
	if err := op.validateSelection(d.com.Workspace); err != nil {
		return InstructionOperation{}, err
	}
	d.operationGeneration, d.operationPending = op.generation, true
	if mutation.kind == instrMetadataValue {
		d.controlsGeneration++ // An older read cannot replace this mutation's acknowledgement.
	}
	return op, nil
}

func (d *Instructions) mutate(mutation instructionMutation) Action {
	op, err := d.beginOperation(mutation)
	if err != nil {
		return ActionCmd{Cmd: util.ReportError(err)}
	}
	ws := d.com.Workspace
	return ActionCmd{Cmd: func() tea.Msg {
		err := op.validateSelection(ws)
		var controlState *config.RuntimeControlState
		if err == nil {
			switch mutation.kind {
			case instrNativeToggle:
				if !op.ownerSet {
					err = fmt.Errorf("instruction provider owner is unavailable")
				} else {
					err = ws.SetProviderToolingInstructions(config.ScopeGlobal, op.owner, mutation.value.(string))
				}
			case instrMode:
				err = ws.SetConfigField(config.ScopeGlobal, "options.instruction_mode", mutation.value)
			case instrSection:
				err = ws.SetConfigField(config.ScopeGlobal, "options.disabled_instruction_sections", mutation.value)
			case instrMetadataValue:
				var state config.RuntimeControlState
				if mutation.remove {
					state, err = ws.RemoveRuntimeControl(context.Background(), config.ScopeGlobal, mutation.controlTarget)
				} else {
					state, err = ws.SetRuntimeControl(context.Background(), config.ScopeGlobal, mutation.controlTarget, mutation.value.(json.RawMessage))
				}
				controlState = &state
			}
		}
		return ActionInstructionMutationCompleted{Operation: op, ControlState: controlState, Err: err}
	}}
}

// FinishOperation completes the execution lifecycle even when its dialog has
// closed. Display state is applied separately only while that dialog is open.
func (d *Instructions) FinishOperation(msg ActionInstructionMutationCompleted) error {
	if err := d.CheckOperation(msg.Operation); err != nil {
		if msg.Operation.generation == d.operationGeneration {
			d.operationPending = false
		}
		return errors.Join(err, msg.Err)
	}
	d.operationPending = false
	if msg.Err == nil && msg.Operation.mutation.kind == instrMetadataValue {
		mutation := msg.Operation.mutation
		if msg.ControlState == nil {
			return fmt.Errorf("runtime control acknowledgement is missing")
		}
		var value json.RawMessage
		if !mutation.remove {
			value = mutation.value.(json.RawMessage)
		}
		return proto.ValidateRuntimeControlAcknowledgement(*msg.ControlState, config.ScopeGlobal, mutation.controlTarget, value, true, !mutation.remove)
	}
	return msg.Err
}

// CancelPreparedEditor cancels work before the terminal editor has launched.
// After editor exit, saved changes must be published regardless of dialog state.
func (d *Instructions) CancelPreparedEditor(op InstructionOperation) {
	if op.Dialog == d && op.generation == d.operationGeneration {
		d.operationPending = false
	}
}

// CompleteOperation applies acknowledged state imperatively on the UI thread.
func (d *Instructions) CompleteOperation(msg ActionInstructionMutationCompleted) error {
	if err := d.FinishOperation(msg); err != nil {
		return err
	}

	mutation := msg.Operation.mutation
	for i := range d.items {
		item := &d.items[i]
		if item.kind != mutation.kind {
			continue
		}
		if mutation.kind == instrMode {
			item.disabled = item.id != mutation.id
		} else if item.id == mutation.id {
			if mutation.kind == instrMetadataValue {
				item.controlState = msg.ControlState
				item.value = runtimeControlDisplayValue(msg.ControlState.Effective)
				item.controlLoading, item.controlError = false, ""
			} else {
				item.disabled = mutation.disabled
			}
		}
	}
	if mutation.kind == instrMetadataValue {
		d.editingMetadata = false
		d.metadataInput.Blur()
	}
	d.updateReplacedSections()
	return nil
}

// ReloadEditedInstructions runs after the editor has returned successfully.
func (op InstructionOperation) ReloadEditedInstructions(ws workspace.Workspace) tea.Cmd {
	return func() tea.Msg {
		err := op.validateSelection(ws)
		if err == nil && op.mutation.kind == instrProviderContextEdit {
			if !op.ownerSet {
				err = fmt.Errorf("instruction provider owner is unavailable")
			} else {
				err = ws.ReloadProviderContextInstructions(context.Background(), op.owner)
			}
		}
		return ActionInstructionMutationCompleted{Operation: op, Err: err}
	}
}

// RebuildAgent re-reads the acknowledged configuration and checks the captured
// selection again before invoking the workspace agent update.
func (op InstructionOperation) RebuildAgent(ws workspace.Workspace) tea.Cmd {
	return func() tea.Msg {
		if err := op.validateSelection(ws); err != nil {
			return util.ReportError(err)()
		}
		if err := ws.UpdateAgentModel(context.Background(), ws.Config().AgentModelState()); err != nil {
			return util.ReportError(err)()
		}
		return nil
	}
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
	kind := instrAction
	if path == d.providerContextPath {
		kind = instrProviderContextEdit
	}
	op, err := d.beginOperation(instructionMutation{kind: kind})
	if err != nil {
		return ActionCmd{Cmd: util.ReportError(err)}
	}
	ws := d.com.Workspace
	return ActionCmd{Cmd: func() tea.Msg {
		prepared := ActionInstructionEditorPrepared{Operation: op}
		if err := op.validateSelection(ws); err != nil {
			prepared.Err = err
			return prepared
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			prepared.Err = err
			return prepared
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := os.WriteFile(path, []byte(initialContent), 0o600); err != nil {
				prepared.Err = err
				return prepared
			}
		} else if err != nil {
			prepared.Err = err
			return prepared
		}
		editorName := os.Getenv("EDITOR")
		if editorName == "" {
			editorName = "vi"
		}
		prepared.Cmd = tea.ExecProcess(exec.CommandContext(context.Background(), editorName, path), func(err error) tea.Msg {
			return ActionInstructionEditorExited{Operation: op, Err: err}
		})
		return prepared
	}}
}

func (d *Instructions) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	frameStyle := t.Dialog.View
	dialogWidth := max(min(d.maxWidth, area.Dx()), 1)
	innerWidth := max(dialogWidth-frameStyle.GetHorizontalFrameSize(), 1)

	rowCount := 0
	for index, item := range d.items {
		switch item.kind {
		case instrHeader:
			rowCount += 2
		case instrMetadataValue:
			rowCount += 2
			if d.editingMetadata && index == d.cursor {
				rowCount++
			}
		default:
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
			label := ansi.Truncate("  ▸ "+item.label, viewportWidth, "…")
			value := ansi.Truncate("    "+runtimeControlRowValue(item), viewportWidth, "…")
			if item.unavailable {
				rows = append(rows, t.Dialog.SecondaryText.Render(label), t.Dialog.SecondaryText.Render(value))
			} else {
				rows = append(rows, d.styledRow(t, index, label), d.styledRow(t, index, value))
			}
			if d.editingMetadata && index == d.cursor {
				input := ansi.Truncate("    "+d.metadataInput.View(), viewportWidth, "…")
				rows = append(rows, d.styledRow(t, index, input))
			}
			itemRows[index] = len(rows) - 1
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
		hintText := "  space: select · e: edit file · esc: close"
		if d.items[d.cursor].kind == instrMetadataValue {
			hintText = "  enter: edit · ctrl+r: reset saved value · esc: close"
			if d.editingMetadata {
				hintText = "  enter: save · ctrl+r: reset · esc: cancel"
			}
		}
		hint := ansi.Truncate(hintText, innerWidth, "…")
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

func sectionDisplayName(id string) string {
	name := strings.ReplaceAll(id, "_", " ")
	if len(name) > 0 {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	return name
}
