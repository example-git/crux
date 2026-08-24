package dialog

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	mcptools "github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/util"
)

const (
	MCPServersID          = "mcp_servers"
	maxMCPFieldPasteBytes = 64 * 1024
)

const (
	mcpFieldName = iota
	mcpFieldType
	mcpFieldCommand
	mcpFieldArgs
	mcpFieldEnv
	mcpFieldURL
	mcpFieldHeaders
	mcpFieldTimeout
	mcpFieldEnabledTools
	mcpFieldDisabledTools
	mcpFieldDisabled
	mcpFieldOAuth
	mcpFieldOAuthClientID
	mcpFieldOAuthClientSecret
	mcpFieldOAuthCallbackPort
	mcpFieldSave
)

var mcpServerNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type mcpServersMode uint8

const (
	mcpServersListMode mcpServersMode = iota
	mcpServersFormMode
)

type mcpServersResultMsg struct {
	name    string
	config  config.MCPConfig
	states  map[string]mcptools.ClientInfo
	saved   bool
	removed bool
	err     error
}

type mcpServersStatesMsg struct {
	states map[string]mcptools.ClientInfo
}

type MCPServers struct {
	com             *common.Common
	help            help.Model
	mode            mcpServersMode
	servers         config.MCPs
	states          map[string]mcptools.ClientInfo
	selected        int
	deleteCandidate string
	editingName     string
	transport       config.MCPType
	disabled        bool
	oauth           bool
	focus           int
	inputs          map[int]textinput.Model
	busy            bool
	status          string
	lastError       string
	keyMap          struct {
		Up      key.Binding
		Down    key.Binding
		Select  key.Binding
		Add     key.Binding
		Delete  key.Binding
		Refresh key.Binding
		Save    key.Binding
		Close   key.Binding
	}
}

var _ Dialog = (*MCPServers)(nil)

func NewMCPServers(com *common.Common) (*MCPServers, tea.Cmd) {
	dialog := &MCPServers{
		com:       com,
		servers:   cloneMCPServers(com.Config().MCP),
		states:    map[string]mcptools.ClientInfo{},
		transport: config.MCPStdio,
		inputs:    map[int]textinput.Model{},
	}
	dialog.help = help.New()
	dialog.help.Styles = com.Styles.DialogHelpStyles()
	dialog.keyMap.Up = key.NewBinding(key.WithKeys("up", "shift+tab"), key.WithHelp("↑", "previous"))
	dialog.keyMap.Down = key.NewBinding(key.WithKeys("down", "tab"), key.WithHelp("↓", "next"))
	dialog.keyMap.Select = key.NewBinding(key.WithKeys("enter", "space"), key.WithHelp("enter", "select"))
	dialog.keyMap.Add = key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "add"))
	dialog.keyMap.Delete = key.NewBinding(key.WithKeys("d"), key.WithHelp("d d", "remove"))
	dialog.keyMap.Refresh = key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "refresh"))
	dialog.keyMap.Save = key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "save"))
	dialog.keyMap.Close = CloseKey
	dialog.initializeInputs()
	return dialog, dialog.refreshCmd()
}

func (*MCPServers) ID() string {
	return MCPServersID
}

func (d *MCPServers) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case mcpServersStatesMsg:
		d.states = msg.states
		d.busy = false
	case mcpServersResultMsg:
		d.busy = false
		if msg.states != nil {
			d.states = msg.states
		}
		if msg.saved {
			d.servers[msg.name] = msg.config
		}
		if msg.err != nil {
			d.lastError = msg.err.Error()
			d.status = "Invalid server configuration or connection"
			return nil
		}
		d.lastError = ""
		if msg.removed {
			delete(d.servers, msg.name)
			delete(d.states, msg.name)
			d.status = "Removed " + msg.name
			d.mode = mcpServersListMode
			d.selected = min(d.selected, len(d.serverNames()))
			return nil
		}
		d.servers[msg.name] = msg.config
		d.status = mcpConnectionFeedback(msg.name, msg.config, d.states[msg.name])
		d.mode = mcpServersListMode
		d.editingName = ""
		return nil
	case tea.PasteMsg:
		return d.handlePaste(msg)
	case tea.KeyPressMsg:
		if d.mode == mcpServersFormMode {
			return d.handleFormKey(msg)
		}
		return d.handleListKey(msg)
	}
	return nil
}

func (d *MCPServers) handlePaste(msg tea.PasteMsg) Action {
	if d.mode != mcpServersFormMode || d.focus == mcpFieldName && d.editingName != "" {
		return nil
	}
	input, ok := d.inputs[d.focus]
	if !ok {
		return nil
	}
	if len(input.Value())+len(msg.Content) > maxMCPFieldPasteBytes {
		return ActionCmd{Cmd: util.ReportWarn("MCP field paste is too large (max 64KB)")}
	}
	var cmd tea.Cmd
	input, cmd = input.Update(msg)
	d.inputs[d.focus] = input
	return ActionCmd{Cmd: cmd}
}

func (d *MCPServers) handleListKey(msg tea.KeyPressMsg) Action {
	names := d.serverNames()
	switch {
	case key.Matches(msg, d.keyMap.Close):
		return ActionClose{}
	case key.Matches(msg, d.keyMap.Add):
		d.openForm("", config.MCPConfig{Type: config.MCPStdio, Timeout: 10})
	case key.Matches(msg, d.keyMap.Refresh):
		d.busy = true
		return ActionCmd{Cmd: d.refreshCmd()}
	case key.Matches(msg, d.keyMap.Up):
		d.selected = (d.selected - 1 + len(names) + 1) % (len(names) + 1)
		d.deleteCandidate = ""
	case key.Matches(msg, d.keyMap.Down):
		d.selected = (d.selected + 1) % (len(names) + 1)
		d.deleteCandidate = ""
	case key.Matches(msg, d.keyMap.Delete):
		if d.selected >= len(names) {
			return nil
		}
		name := names[d.selected]
		if d.deleteCandidate != name {
			d.deleteCandidate = name
			d.status = "Press d again to remove " + name
			return nil
		}
		d.busy = true
		d.deleteCandidate = ""
		return ActionCmd{Cmd: d.removeCmd(name)}
	case key.Matches(msg, d.keyMap.Select):
		if d.selected == len(names) {
			d.openForm("", config.MCPConfig{Type: config.MCPStdio, Timeout: 10})
			return nil
		}
		name := names[d.selected]
		d.openForm(name, d.servers[name])
	}
	return nil
}

func (d *MCPServers) handleFormKey(msg tea.KeyPressMsg) Action {
	fields := d.formFields()
	switch {
	case key.Matches(msg, d.keyMap.Close):
		d.mode = mcpServersListMode
		d.lastError = ""
		d.editingName = ""
		return nil
	case key.Matches(msg, d.keyMap.Save):
		return d.save()
	case key.Matches(msg, d.keyMap.Up):
		d.setFormFocus(fields[(slices.Index(fields, d.focus)-1+len(fields))%len(fields)])
		return nil
	case key.Matches(msg, d.keyMap.Down):
		d.setFormFocus(fields[(slices.Index(fields, d.focus)+1)%len(fields)])
		return nil
	case (msg.String() == "left" || msg.String() == "right" || msg.String() == "space") && isMCPChoiceField(d.focus):
		switch d.focus {
		case mcpFieldType:
			d.cycleTransport(msg.String() != "left")
		case mcpFieldDisabled:
			d.disabled = !d.disabled
		case mcpFieldOAuth:
			d.oauth = !d.oauth
		}
		return nil
	case msg.String() == "enter":
		switch d.focus {
		case mcpFieldType:
			d.cycleTransport(true)
		case mcpFieldDisabled:
			d.disabled = !d.disabled
		case mcpFieldOAuth:
			d.oauth = !d.oauth
		case mcpFieldSave:
			return d.save()
		default:
			d.setFormFocus(fields[(slices.Index(fields, d.focus)+1)%len(fields)])
		}
		return nil
	}
	input, ok := d.inputs[d.focus]
	if !ok || d.focus == mcpFieldName && d.editingName != "" {
		return nil
	}
	var cmd tea.Cmd
	input, cmd = input.Update(msg)
	d.inputs[d.focus] = input
	return ActionCmd{Cmd: cmd}
}

func isMCPChoiceField(field int) bool {
	return field == mcpFieldType || field == mcpFieldDisabled || field == mcpFieldOAuth
}

func (d *MCPServers) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	if d.mode == mcpServersFormMode {
		return d.drawForm(scr, area)
	}
	return d.drawList(scr, area)
}

func (d *MCPServers) drawList(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(78, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := max(0, width-t.Dialog.View.GetHorizontalFrameSize())
	names := d.serverNames()
	rows := make([]string, 0, len(names)+4)
	for i, name := range names {
		cfg := d.servers[name]
		state := mcpStateLabel(cfg, d.states[name])
		line := name + "  " + t.Dialog.SecondaryText.Render(string(cfg.Type)+" · "+state)
		line = ansi.Truncate(line, innerWidth, "…")
		rows = append(rows, d.rowStyle(i).Render(line))
	}
	rows = append(rows, d.rowStyle(len(names)).Render("+ Add MCP server"))
	if d.busy {
		rows = append(rows, "", t.Dialog.SecondaryText.Render("Checking MCP servers…"))
	} else if d.status != "" {
		rows = append(rows, "", t.Dialog.SecondaryText.Render(ansi.Truncate(d.status, innerWidth, "…")))
	}
	if d.lastError != "" {
		rows = append(rows, t.Dialog.TitleError.Render(ansi.Truncate(d.lastError, innerWidth, "…")))
	}
	rc := NewRenderContext(t, width)
	rc.Title = "MCP Servers"
	rc.AddPart(lipgloss.JoinVertical(lipgloss.Left, rows...))
	rc.Help = renderDialogHelp(t, &d.help, d, innerWidth)
	DrawCenter(scr, area, rc.Render())
	return nil
}

func (d *MCPServers) drawForm(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	width := max(0, min(88, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	innerWidth := max(0, width-t.Dialog.View.GetHorizontalFrameSize())
	labelWidth := 16
	inputWidth := max(1, innerWidth-labelWidth)
	fields := d.formFields()
	rows := make([]string, 0, len(fields)+2)
	cursorRow := -1
	for row, field := range fields {
		if input, ok := d.inputs[field]; ok {
			input.SetWidth(inputWidth)
			d.inputs[field] = input
			value := input.View()
			if field == mcpFieldName && d.editingName != "" {
				value = t.Dialog.SecondaryText.Render(d.editingName)
			}
			rows = append(rows, d.inputLine(field, mcpFieldLabel(field), value))
			if field == d.focus && (field != mcpFieldName || d.editingName == "") {
				cursorRow = row
			}
			continue
		}
		rows = append(rows, d.rowStyle(field).Render(d.formChoice(field)))
	}
	if d.busy {
		rows = append(rows, "", t.Dialog.SecondaryText.Render("Saving and validating connection…"))
	}
	if d.lastError != "" {
		rows = append(rows, "", t.Dialog.TitleError.Render(ansi.Truncate(d.lastError, innerWidth, "…")))
	}
	rc := NewRenderContext(t, width)
	rc.Title = "MCP Server"
	rc.TitleInfo = t.Dialog.SecondaryText.Render("Project config")
	rc.AddPart(lipgloss.JoinVertical(lipgloss.Left, rows...))
	rc.Help = renderDialogHelp(t, &d.help, d, innerWidth)
	view := rc.Render()
	if cursorRow < 0 {
		DrawCenter(scr, area, view)
		return nil
	}
	input := d.inputs[d.focus]
	cursor := InputCursor(t, input.Cursor())
	if cursor != nil {
		cursor.X += labelWidth
		cursor.Y += cursorRow
	}
	DrawCenterCursor(scr, area, view, cursor)
	return cursor
}

func (d *MCPServers) ShortHelp() []key.Binding {
	if d.mode == mcpServersFormMode {
		return []key.Binding{d.keyMap.Up, d.keyMap.Down, d.keyMap.Select, d.keyMap.Save, d.keyMap.Close}
	}
	return []key.Binding{d.keyMap.Up, d.keyMap.Down, d.keyMap.Select, d.keyMap.Add, d.keyMap.Delete, d.keyMap.Refresh, d.keyMap.Close}
}

func (d *MCPServers) FullHelp() [][]key.Binding {
	return [][]key.Binding{d.ShortHelp()}
}

func (d *MCPServers) initializeInputs() {
	d.inputs[mcpFieldName] = d.newInput("github")
	d.inputs[mcpFieldCommand] = d.newInput("npx")
	d.inputs[mcpFieldArgs] = d.newInput("-y, @modelcontextprotocol/server-filesystem")
	d.inputs[mcpFieldEnv] = d.newInput("TOKEN=$MCP_TOKEN")
	d.inputs[mcpFieldURL] = d.newInput("https://example.com/mcp")
	d.inputs[mcpFieldHeaders] = d.newInput("Authorization=Bearer $MCP_TOKEN")
	d.inputs[mcpFieldTimeout] = d.newInput("10")
	d.inputs[mcpFieldEnabledTools] = d.newInput("optional allow list")
	d.inputs[mcpFieldDisabledTools] = d.newInput("optional deny list")
	d.inputs[mcpFieldOAuthClientID] = d.newInput("optional client ID")
	d.inputs[mcpFieldOAuthClientSecret] = d.newInput("optional client secret or $VAR")
	d.inputs[mcpFieldOAuthCallbackPort] = d.newInput("optional port")
}

func (d *MCPServers) newInput(placeholder string) textinput.Model {
	input := textinput.New()
	input.SetVirtualCursor(true)
	input.Placeholder = placeholder
	input.SetStyles(d.com.Styles.TextInput)
	return input
}

func (d *MCPServers) openForm(name string, cfg config.MCPConfig) {
	d.mode = mcpServersFormMode
	d.editingName = name
	d.transport = cfg.Type
	if d.transport == "" {
		d.transport = config.MCPStdio
	}
	d.disabled = cfg.Disabled
	d.oauth = cfg.OAuth
	d.setInputValue(mcpFieldName, name)
	d.setInputValue(mcpFieldCommand, cfg.Command)
	d.setInputValue(mcpFieldArgs, strings.Join(cfg.Args, ", "))
	d.setInputValue(mcpFieldEnv, joinMCPPairs(cfg.Env))
	d.setInputValue(mcpFieldURL, cfg.URL)
	d.setInputValue(mcpFieldHeaders, joinMCPPairs(cfg.Headers))
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10
	}
	d.setInputValue(mcpFieldTimeout, strconv.Itoa(timeout))
	d.setInputValue(mcpFieldEnabledTools, strings.Join(cfg.EnabledTools, ", "))
	d.setInputValue(mcpFieldDisabledTools, strings.Join(cfg.DisabledTools, ", "))
	d.setInputValue(mcpFieldOAuthClientID, cfg.OAuthClientID)
	d.setInputValue(mcpFieldOAuthClientSecret, cfg.OAuthClientSecret)
	callbackPort := ""
	if cfg.OAuthCallbackPort > 0 {
		callbackPort = strconv.Itoa(cfg.OAuthCallbackPort)
	}
	d.setInputValue(mcpFieldOAuthCallbackPort, callbackPort)
	d.lastError = ""
	d.setFormFocus(d.formFields()[0])
}

func (d *MCPServers) setInputValue(field int, value string) {
	input := d.inputs[field]
	input.SetValue(value)
	d.inputs[field] = input
}

func (d *MCPServers) formFields() []int {
	fields := []int{mcpFieldName, mcpFieldType}
	if d.transport == config.MCPStdio {
		fields = append(fields, mcpFieldCommand, mcpFieldArgs, mcpFieldEnv)
	} else {
		fields = append(fields, mcpFieldURL, mcpFieldHeaders)
	}
	fields = append(fields, mcpFieldTimeout, mcpFieldEnabledTools, mcpFieldDisabledTools, mcpFieldDisabled)
	if d.transport == config.MCPHttp {
		fields = append(fields, mcpFieldOAuth)
		if d.oauth {
			fields = append(fields, mcpFieldOAuthClientID, mcpFieldOAuthClientSecret, mcpFieldOAuthCallbackPort)
		}
	}
	return append(fields, mcpFieldSave)
}

func (d *MCPServers) setFormFocus(field int) {
	d.focus = field
	for id, input := range d.inputs {
		input.Blur()
		d.inputs[id] = input
	}
	if input, ok := d.inputs[field]; ok && (field != mcpFieldName || d.editingName == "") {
		input.Focus()
		d.inputs[field] = input
	}
}

func (d *MCPServers) cycleTransport(forward bool) {
	transports := []config.MCPType{config.MCPStdio, config.MCPHttp, config.MCPSSE}
	index := slices.Index(transports, d.transport)
	if forward {
		index = (index + 1) % len(transports)
	} else {
		index = (index - 1 + len(transports)) % len(transports)
	}
	d.transport = transports[index]
	if d.transport != config.MCPHttp {
		d.oauth = false
	}
	d.setFormFocus(mcpFieldType)
}

func (d *MCPServers) rowStyle(row int) lipgloss.Style {
	if d.mode == mcpServersFormMode && d.focus == row || d.mode == mcpServersListMode && d.selected == row {
		return d.com.Styles.Dialog.SelectedItem
	}
	return d.com.Styles.Dialog.NormalItem
}

func (d *MCPServers) inputLine(field int, label, value string) string {
	style := d.com.Styles.Dialog.Arguments.InputLabelBlurred
	if field == d.focus {
		style = d.com.Styles.Dialog.Arguments.InputLabelFocused
	}
	return style.Width(16).Render(label+":") + value
}

func (d *MCPServers) formChoice(field int) string {
	check := func(enabled bool) string {
		if enabled {
			return "[✓]"
		}
		return "[ ]"
	}
	switch field {
	case mcpFieldType:
		return " Transport: " + string(d.transport) + "  (←/→)"
	case mcpFieldDisabled:
		return " " + check(d.disabled) + " Save disabled"
	case mcpFieldOAuth:
		return " " + check(d.oauth) + " OAuth 2.1"
	case mcpFieldSave:
		return " Save and validate"
	default:
		return ""
	}
}

func mcpFieldLabel(field int) string {
	switch field {
	case mcpFieldName:
		return "Name"
	case mcpFieldCommand:
		return "Command"
	case mcpFieldArgs:
		return "Arguments"
	case mcpFieldEnv:
		return "Environment"
	case mcpFieldURL:
		return "URL"
	case mcpFieldHeaders:
		return "Headers"
	case mcpFieldTimeout:
		return "Timeout seconds"
	case mcpFieldEnabledTools:
		return "Enabled tools"
	case mcpFieldDisabledTools:
		return "Disabled tools"
	case mcpFieldOAuthClientID:
		return "OAuth client ID"
	case mcpFieldOAuthClientSecret:
		return "OAuth secret"
	case mcpFieldOAuthCallbackPort:
		return "OAuth port"
	default:
		return ""
	}
}

func (d *MCPServers) save() Action {
	if d.busy {
		return nil
	}
	name, cfg, err := d.formConfig()
	if err != nil {
		d.lastError = err.Error()
		return nil
	}
	d.busy = true
	d.lastError = ""
	return ActionCmd{Cmd: d.saveCmd(name, cfg)}
}

func (d *MCPServers) formConfig() (string, config.MCPConfig, error) {
	name := strings.TrimSpace(d.inputs[mcpFieldName].Value())
	if d.editingName != "" {
		name = d.editingName
	}
	if !mcpServerNamePattern.MatchString(name) {
		return "", config.MCPConfig{}, fmt.Errorf("name must contain only letters, numbers, underscores, or hyphens")
	}
	timeout, err := optionalMCPInt(d.inputs[mcpFieldTimeout].Value(), "timeout")
	if err != nil {
		return "", config.MCPConfig{}, err
	}
	cfg := config.MCPConfig{
		Type:          d.transport,
		Timeout:       timeout,
		Disabled:      d.disabled,
		EnabledTools:  splitMCPValues(d.inputs[mcpFieldEnabledTools].Value()),
		DisabledTools: splitMCPValues(d.inputs[mcpFieldDisabledTools].Value()),
	}
	switch d.transport {
	case config.MCPStdio:
		env, pairErr := parseMCPPairs(d.inputs[mcpFieldEnv].Value())
		if pairErr != nil {
			return "", config.MCPConfig{}, fmt.Errorf("environment: %w", pairErr)
		}
		cfg.Command = strings.TrimSpace(d.inputs[mcpFieldCommand].Value())
		cfg.Args = splitMCPValues(d.inputs[mcpFieldArgs].Value())
		cfg.Env = env
		if cfg.Command == "" {
			return "", config.MCPConfig{}, fmt.Errorf("command is required for stdio servers")
		}
	case config.MCPHttp, config.MCPSSE:
		headers, pairErr := parseMCPPairs(d.inputs[mcpFieldHeaders].Value())
		if pairErr != nil {
			return "", config.MCPConfig{}, fmt.Errorf("headers: %w", pairErr)
		}
		cfg.URL = strings.TrimSpace(d.inputs[mcpFieldURL].Value())
		cfg.Headers = headers
		parsed, parseErr := url.ParseRequestURI(cfg.URL)
		if parseErr != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", config.MCPConfig{}, fmt.Errorf("URL must be an absolute http or https URL")
		}
		if d.transport == config.MCPHttp && d.oauth {
			callbackPort, callbackErr := optionalMCPInt(d.inputs[mcpFieldOAuthCallbackPort].Value(), "OAuth callback port")
			if callbackErr != nil {
				return "", config.MCPConfig{}, callbackErr
			}
			cfg.OAuth = true
			cfg.OAuthClientID = strings.TrimSpace(d.inputs[mcpFieldOAuthClientID].Value())
			cfg.OAuthClientSecret = strings.TrimSpace(d.inputs[mcpFieldOAuthClientSecret].Value())
			cfg.OAuthCallbackPort = callbackPort
		}
	default:
		return "", config.MCPConfig{}, fmt.Errorf("unsupported transport %q", d.transport)
	}
	return name, cfg, nil
}

func (d *MCPServers) saveCmd(name string, cfg config.MCPConfig) tea.Cmd {
	workspace := d.com.Workspace
	return func() tea.Msg {
		err := workspace.SetConfigField(config.ScopeWorkspace, "mcp."+name, cfg)
		saved := err == nil
		if err == nil {
			err = waitForMCPValidation(context.Background(), workspace.MCPGetStates, name, cfg)
		}
		return mcpServersResultMsg{name: name, config: cfg, states: workspace.MCPGetStates(), saved: saved, err: err}
	}
}

func (d *MCPServers) removeCmd(name string) tea.Cmd {
	workspace := d.com.Workspace
	return func() tea.Msg {
		err := workspace.RemoveConfigField(config.ScopeWorkspace, "mcp."+name)
		return mcpServersResultMsg{name: name, states: workspace.MCPGetStates(), removed: err == nil, err: err}
	}
}

func (d *MCPServers) refreshCmd() tea.Cmd {
	workspace := d.com.Workspace
	return func() tea.Msg {
		return mcpServersStatesMsg{states: workspace.MCPGetStates()}
	}
}

func (d *MCPServers) serverNames() []string {
	names := make([]string, 0, len(d.servers))
	for name := range d.servers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func cloneMCPServers(servers config.MCPs) config.MCPs {
	result := make(config.MCPs, len(servers))
	maps.Copy(result, servers)
	return result
}

func splitMCPValues(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.HasPrefix(value, "[") {
		var result []string
		if json.Unmarshal([]byte(value), &result) == nil {
			return result
		}
	}
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' })
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func parseMCPPairs(value string) (map[string]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if strings.HasPrefix(value, "{") {
		var result map[string]string
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return nil, fmt.Errorf("invalid JSON object: %w", err)
		}
		return result, nil
	}
	result := map[string]string{}
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' }) {
		key, val, ok := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("expected KEY=value entries")
		}
		result[key] = strings.TrimSpace(val)
	}
	return result, nil
}

func joinMCPPairs(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values[key])
	}
	return strings.Join(parts, ", ")
}

func optionalMCPInt(value, label string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	result, err := strconv.Atoi(value)
	if err != nil || result < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", label)
	}
	return result, nil
}

func waitForMCPValidation(ctx context.Context, states func() map[string]mcptools.ClientInfo, name string, cfg config.MCPConfig) error {
	if cfg.Disabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(max(cfg.Timeout, 10)+2)*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, ok := states()[name]; ok {
			switch info.State {
			case mcptools.StateConnected, mcptools.StateNeedsAuth:
				return nil
			case mcptools.StateError:
				if info.Error != nil {
					return info.Error
				}
				return fmt.Errorf("MCP server failed to initialize")
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out validating MCP server: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func mcpStateLabel(cfg config.MCPConfig, info mcptools.ClientInfo) string {
	if cfg.Disabled {
		return "disabled"
	}
	if info.Name == "" {
		return "not started"
	}
	return info.State.String()
}

func mcpConnectionFeedback(name string, cfg config.MCPConfig, info mcptools.ClientInfo) string {
	if cfg.Disabled {
		return "Saved " + name + " as disabled"
	}
	switch info.State {
	case mcptools.StateConnected:
		return fmt.Sprintf("Connected %s · %d tools · %d prompts", name, info.Counts.Tools, info.Counts.Prompts)
	case mcptools.StateNeedsAuth:
		return "Saved " + name + " · authentication required"
	default:
		return "Saved " + name
	}
}
