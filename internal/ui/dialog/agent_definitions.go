package dialog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/ui/common"
)

const AgentDefinitionsID = "agent_definitions"

const (
	agentDefinitionFieldName = iota
	agentDefinitionFieldScope
	agentDefinitionFieldDescription
	agentDefinitionFieldModel
	agentDefinitionFieldTools
	agentDefinitionFieldScript
	agentDefinitionFieldScriptPath
	agentDefinitionFieldScriptTimeout
	agentDefinitionFieldScriptVariables
	agentDefinitionFieldSave
)

type agentDefinitionResultMsg struct {
	path string
	err  error
}

type AgentDefinitions struct {
	com       *common.Common
	help      help.Model
	inputs    map[int]textinput.Model
	focus     int
	scope     string
	models    []string
	model     int
	script    bool
	busy      bool
	lastError string
	keyMap    struct {
		Up     key.Binding
		Down   key.Binding
		Select key.Binding
		Save   key.Binding
		Close  key.Binding
	}
}

var _ Dialog = (*AgentDefinitions)(nil)

func NewAgentDefinitions(com *common.Common) *AgentDefinitions {
	dialog := &AgentDefinitions{
		com:    com,
		inputs: map[int]textinput.Model{},
		scope:  "project",
		models: configuredAgentModels(com.Config()),
	}
	dialog.help = help.New()
	dialog.help.Styles = com.Styles.DialogHelpStyles()
	dialog.keyMap.Up = key.NewBinding(key.WithKeys("up", "shift+tab"), key.WithHelp("↑", "previous"))
	dialog.keyMap.Down = key.NewBinding(key.WithKeys("down", "tab"), key.WithHelp("↓", "next"))
	dialog.keyMap.Select = key.NewBinding(key.WithKeys("enter", "space"), key.WithHelp("enter", "select"))
	dialog.keyMap.Save = key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "create"))
	dialog.keyMap.Close = CloseKey
	dialog.inputs[agentDefinitionFieldName] = dialog.newInput("code-reviewer", 64)
	dialog.inputs[agentDefinitionFieldDescription] = dialog.newInput("Reviews changes for correctness", 256)
	dialog.inputs[agentDefinitionFieldTools] = dialog.newInput("none, *, or comma-separated tool names", 4096)
	dialog.inputs[agentDefinitionFieldScriptPath] = dialog.newInput("./scripts/task.py", 1024)
	dialog.inputs[agentDefinitionFieldScriptTimeout] = dialog.newInput("2m", 32)
	dialog.inputs[agentDefinitionFieldScriptVariables] = dialog.newInput(`{"input":{"required":true},"mode":{"value":"fast"}}`, 4096)
	dialog.selectDefaultModel()
	dialog.setFocus(agentDefinitionFieldName)
	return dialog
}

func (*AgentDefinitions) ID() string {
	return AgentDefinitionsID
}

func (d *AgentDefinitions) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case agentDefinitionResultMsg:
		d.busy = false
		if msg.path != "" {
			return ActionAgentDefinitionCreated{Path: msg.path, RefreshErr: msg.err}
		}
		if msg.err != nil {
			d.lastError = msg.err.Error()
		}
		return nil
	case tea.KeyPressMsg:
		return d.handleKey(msg)
	}
	input, ok := d.inputs[d.focus]
	if !ok || d.busy {
		return nil
	}
	var cmd tea.Cmd
	input, cmd = input.Update(msg)
	d.inputs[d.focus] = input
	return ActionCmd{Cmd: cmd}
}

func (d *AgentDefinitions) handleKey(msg tea.KeyPressMsg) Action {
	fields := d.fields()
	switch {
	case key.Matches(msg, d.keyMap.Close):
		return ActionClose{}
	case key.Matches(msg, d.keyMap.Save):
		return d.save()
	case key.Matches(msg, d.keyMap.Up):
		d.setFocus(fields[(slices.Index(fields, d.focus)-1+len(fields))%len(fields)])
		return nil
	case key.Matches(msg, d.keyMap.Down):
		d.setFocus(fields[(slices.Index(fields, d.focus)+1)%len(fields)])
		return nil
	case (msg.String() == "left" || msg.String() == "right" || msg.String() == "space") && d.isChoiceField():
		d.cycleChoice(msg.String() != "left")
		return nil
	case msg.String() == "enter":
		if d.isChoiceField() {
			d.cycleChoice(true)
		} else if d.focus == agentDefinitionFieldSave {
			return d.save()
		} else {
			d.setFocus(fields[(slices.Index(fields, d.focus)+1)%len(fields)])
		}
		return nil
	}
	input, ok := d.inputs[d.focus]
	if !ok || d.busy {
		return nil
	}
	var cmd tea.Cmd
	input, cmd = input.Update(msg)
	d.inputs[d.focus] = input
	return ActionCmd{Cmd: cmd}
}

func (d *AgentDefinitions) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(88, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := max(0, width-t.Dialog.View.GetHorizontalFrameSize())
	labelWidth := 14
	inputWidth := max(1, innerWidth-labelWidth)
	rows := make([]string, 0, len(d.fields())+3)
	cursorRow := -1
	for row, field := range d.fields() {
		if input, ok := d.inputs[field]; ok {
			input.SetWidth(inputWidth)
			d.inputs[field] = input
			rows = append(rows, d.inputLine(field, agentDefinitionFieldLabel(field), input.View(), labelWidth))
			if field == d.focus {
				cursorRow = row
			}
			continue
		}
		rows = append(rows, d.rowStyle(field).Render(d.choiceLine(field)))
	}
	if d.busy {
		rows = append(rows, "", t.Dialog.SecondaryText.Render("Creating agent definition…"))
	}
	if d.lastError != "" {
		rows = append(rows, "", t.Dialog.TitleError.Render(ansi.Truncate(d.lastError, innerWidth, "…")))
	}
	rc := NewRenderContext(t, width)
	rc.Title = "Create Agent Definition"
	rc.TitleInfo = t.Dialog.SecondaryText.Render("Markdown preset")
	rc.AddPart(lipgloss.JoinVertical(lipgloss.Left, rows...))
	rc.Help = renderDialogHelp(t, &d.help, d, innerWidth)
	view := rc.Render()
	if cursorRow < 0 {
		DrawCenter(scr, area, view)
		return nil
	}
	cursor := InputCursor(t, d.inputs[d.focus].Cursor())
	if cursor != nil {
		cursor.X += labelWidth
		cursor.Y += cursorRow
	}
	DrawCenterCursor(scr, area, view, cursor)
	return cursor
}

func (d *AgentDefinitions) ShortHelp() []key.Binding {
	return []key.Binding{d.keyMap.Up, d.keyMap.Down, d.keyMap.Select, d.keyMap.Save, d.keyMap.Close}
}

func (d *AgentDefinitions) FullHelp() [][]key.Binding {
	return [][]key.Binding{d.ShortHelp()}
}

func (d *AgentDefinitions) newInput(placeholder string, limit int) textinput.Model {
	input := textinput.New()
	input.SetVirtualCursor(true)
	input.Placeholder = placeholder
	input.CharLimit = limit
	input.SetStyles(d.com.Styles.TextInput)
	return input
}

func (d *AgentDefinitions) fields() []int {
	fields := []int{
		agentDefinitionFieldName,
		agentDefinitionFieldScope,
		agentDefinitionFieldDescription,
		agentDefinitionFieldModel,
		agentDefinitionFieldTools,
		agentDefinitionFieldScript,
	}
	if d.script {
		fields = append(fields, agentDefinitionFieldScriptPath, agentDefinitionFieldScriptTimeout, agentDefinitionFieldScriptVariables)
	}
	return append(fields, agentDefinitionFieldSave)
}

func (d *AgentDefinitions) setFocus(field int) {
	d.focus = field
	for current, input := range d.inputs {
		if current == field {
			input.Focus()
		} else {
			input.Blur()
		}
		d.inputs[current] = input
	}
}

func (d *AgentDefinitions) isChoiceField() bool {
	return d.focus == agentDefinitionFieldScope || d.focus == agentDefinitionFieldModel || d.focus == agentDefinitionFieldScript
}

func (d *AgentDefinitions) cycleChoice(forward bool) {
	switch d.focus {
	case agentDefinitionFieldScope:
		if d.scope == "project" {
			d.scope = "user"
		} else {
			d.scope = "project"
		}
	case agentDefinitionFieldScript:
		d.script = !d.script
	case agentDefinitionFieldModel:
		if len(d.models) == 0 {
			return
		}
		if forward {
			d.model = (d.model + 1) % len(d.models)
		} else {
			d.model = (d.model - 1 + len(d.models)) % len(d.models)
		}
	}
}

func (d *AgentDefinitions) rowStyle(field int) lipgloss.Style {
	if field == d.focus {
		return d.com.Styles.Dialog.SelectedItem
	}
	return d.com.Styles.Dialog.NormalItem
}

func (d *AgentDefinitions) inputLine(field int, label, value string, labelWidth int) string {
	style := d.com.Styles.Dialog.Arguments.InputLabelBlurred
	if field == d.focus {
		style = d.com.Styles.Dialog.Arguments.InputLabelFocused
	}
	return style.Width(labelWidth).Render(label+":") + value
}

func (d *AgentDefinitions) choiceLine(field int) string {
	switch field {
	case agentDefinitionFieldScope:
		return " Scope: " + d.scope + "  (←/→)"
	case agentDefinitionFieldModel:
		model := "no configured models"
		if len(d.models) > 0 {
			model = d.models[d.model]
		}
		return " Model: " + model + "  (←/→)"
	case agentDefinitionFieldScript:
		state := "disabled"
		if d.script {
			state = "enabled"
		}
		return " Script: " + state + "  (←/→)"
	case agentDefinitionFieldSave:
		return " Create definition"
	default:
		return ""
	}
}

func (d *AgentDefinitions) save() Action {
	if d.busy {
		return nil
	}
	request, err := d.request()
	if err != nil {
		d.lastError = err.Error()
		return nil
	}
	d.busy = true
	d.lastError = ""
	return ActionCmd{Cmd: func() tea.Msg {
		path, err := d.com.Workspace.CreateAgentDefinition(context.Background(), request)
		if err == nil {
			err = d.com.Workspace.UpdateAgentModel(context.Background())
		}
		return agentDefinitionResultMsg{path: path, err: err}
	}}
}

func (d *AgentDefinitions) request() (proto.CreateAgentDefinitionRequest, error) {
	if len(d.models) == 0 {
		return proto.CreateAgentDefinitionRequest{}, errors.New("no configured models are available")
	}
	name := strings.TrimSpace(d.inputs[agentDefinitionFieldName].Value())
	if name == "" {
		return proto.CreateAgentDefinitionRequest{}, errors.New("agent type name is required")
	}
	description := strings.TrimSpace(d.inputs[agentDefinitionFieldDescription].Value())
	if description == "" {
		return proto.CreateAgentDefinitionRequest{}, errors.New("description is required")
	}
	selectedTools, err := parseAgentDefinitionTools(d.inputs[agentDefinitionFieldTools].Value())
	if err != nil {
		return proto.CreateAgentDefinitionRequest{}, err
	}
	var script *proto.AgentDefinitionScript
	if d.script {
		scriptPath := strings.TrimSpace(d.inputs[agentDefinitionFieldScriptPath].Value())
		if scriptPath == "" {
			return proto.CreateAgentDefinitionRequest{}, errors.New("script path is required")
		}
		variables, err := parseAgentDefinitionScriptVariables(d.inputs[agentDefinitionFieldScriptVariables].Value())
		if err != nil {
			return proto.CreateAgentDefinitionRequest{}, err
		}
		script = &proto.AgentDefinitionScript{
			Path:      scriptPath,
			Timeout:   strings.TrimSpace(d.inputs[agentDefinitionFieldScriptTimeout].Value()),
			Variables: variables,
		}
		if !slices.Contains(selectedTools, "*") && !slices.Contains(selectedTools, "script") {
			selectedTools = append(selectedTools, "script")
		}
	} else if slices.Contains(selectedTools, "script") {
		return proto.CreateAgentDefinitionRequest{}, errors.New("enable script configuration to use the script tool")
	}
	return proto.CreateAgentDefinitionRequest{
		Scope:       d.scope,
		Name:        name,
		Description: description,
		Model:       d.models[d.model],
		Tools:       selectedTools,
		Script:      script,
	}, nil
}

func (d *AgentDefinitions) selectDefaultModel() {
	selected := d.com.Config().Models[config.SelectedModelTypeLarge]
	model := selected.Provider + "/" + selected.Model
	if index := slices.Index(d.models, model); index >= 0 {
		d.model = index
	}
}

func configuredAgentModels(cfg *config.Config) []string {
	if cfg == nil || cfg.Providers == nil {
		return nil
	}
	models := make([]string, 0)
	for providerID, provider := range cfg.Providers.Seq2() {
		if !cfg.IsProviderAvailable(providerID) {
			continue
		}
		for _, model := range provider.Models {
			if model.ID != "" {
				models = append(models, providerID+"/"+model.ID)
			}
		}
	}
	slices.Sort(models)
	return slices.Compact(models)
}

func parseAgentDefinitionTools(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "none" {
		return []string{}, nil
	}
	parts := strings.Split(value, ",")
	tools := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			return nil, errors.New("tools cannot contain an empty name")
		}
		if _, ok := seen[name]; ok {
			return nil, errors.New("tool " + name + " is listed more than once")
		}
		seen[name] = struct{}{}
		tools = append(tools, name)
	}
	if slices.Contains(tools, "*") && len(tools) != 1 {
		return nil, errors.New("wildcard tool must be the only tool")
	}
	return tools, nil
}

func parseAgentDefinitionScriptVariables(value string) (map[string]proto.AgentDefinitionScriptVariable, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return map[string]proto.AgentDefinitionScriptVariable{}, nil
	}
	variables := make(map[string]proto.AgentDefinitionScriptVariable)
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&variables); err != nil {
		return nil, fmt.Errorf("invalid script variables JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("invalid script variables JSON: multiple values are not supported")
	}
	return variables, nil
}

func agentDefinitionFieldLabel(field int) string {
	switch field {
	case agentDefinitionFieldName:
		return "Agent type"
	case agentDefinitionFieldDescription:
		return "Description"
	case agentDefinitionFieldTools:
		return "Tools"
	case agentDefinitionFieldScriptPath:
		return "Script path"
	case agentDefinitionFieldScriptTimeout:
		return "Timeout"
	case agentDefinitionFieldScriptVariables:
		return "Variables JSON"
	default:
		return ""
	}
}
