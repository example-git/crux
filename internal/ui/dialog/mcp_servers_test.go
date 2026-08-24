package dialog

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	mcptools "github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

type mcpServersTestWorkspace struct {
	workspace.Workspace
	cfg           *config.Config
	states        map[string]mcptools.ClientInfo
	setFields     map[string]any
	removed       []string
	stateCalls    int
	setErr        error
	removeErr     error
	stateAfterSet map[string]mcptools.ClientInfo
}

func (w *mcpServersTestWorkspace) Config() *config.Config {
	return w.cfg
}

func (w *mcpServersTestWorkspace) SetConfigField(_ config.Scope, key string, value any) error {
	if w.setErr != nil {
		return w.setErr
	}
	w.setFields[key] = value
	if w.stateAfterSet != nil {
		w.states = w.stateAfterSet
	}
	return nil
}

func (w *mcpServersTestWorkspace) RemoveConfigField(_ config.Scope, key string) error {
	if w.removeErr != nil {
		return w.removeErr
	}
	w.removed = append(w.removed, key)
	return nil
}

func (w *mcpServersTestWorkspace) MCPGetStates() map[string]mcptools.ClientInfo {
	w.stateCalls++
	return w.states
}

func newMCPServersTestDialog(t *testing.T, servers config.MCPs) (*MCPServers, tea.Cmd, *mcpServersTestWorkspace) {
	t.Helper()
	ws := &mcpServersTestWorkspace{
		cfg:       &config.Config{MCP: servers},
		states:    map[string]mcptools.ClientInfo{},
		setFields: map[string]any{},
	}
	theme := styles.ThemeForProvider("")
	dialog, cmd := NewMCPServers(&common.Common{Workspace: ws, Styles: &theme})
	return dialog, cmd, ws
}

func TestMCPServersLoadsWithoutSynchronousStateIO(t *testing.T) {
	dialog, cmd, ws := newMCPServersTestDialog(t, config.MCPs{
		"zeta":  {Type: config.MCPHttp, URL: "https://example.com/mcp"},
		"alpha": {Type: config.MCPStdio, Command: "alpha"},
	})

	if ws.stateCalls != 0 {
		t.Fatalf("constructor state calls = %d, want 0", ws.stateCalls)
	}
	if got := dialog.serverNames(); !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Fatalf("server names = %#v", got)
	}
	dialog.HandleMsg(cmd())
	if ws.stateCalls != 1 {
		t.Fatalf("refresh state calls = %d, want 1", ws.stateCalls)
	}
}

func TestMCPServersFormBuildsTransportSpecificConfigs(t *testing.T) {
	dialog, _, _ := newMCPServersTestDialog(t, nil)
	dialog.openForm("", config.MCPConfig{Type: config.MCPStdio})
	dialog.setInputValue(mcpFieldName, "filesystem")
	dialog.setInputValue(mcpFieldCommand, "npx")
	dialog.setInputValue(mcpFieldArgs, `["-y", "@modelcontextprotocol/server-filesystem"]`)
	dialog.setInputValue(mcpFieldEnv, `{"TOKEN":"$MCP_TOKEN"}`)
	dialog.setInputValue(mcpFieldEnabledTools, "read, list")
	dialog.setInputValue(mcpFieldOAuthCallbackPort, "not-an-integer")
	dialog.oauth = true

	name, cfg, err := dialog.formConfig()
	if err != nil {
		t.Fatalf("stdio config error: %v", err)
	}
	if name != "filesystem" || cfg.Type != config.MCPStdio || cfg.Command != "npx" {
		t.Fatalf("stdio config = %#v", cfg)
	}
	if !reflect.DeepEqual(cfg.Args, []string{"-y", "@modelcontextprotocol/server-filesystem"}) {
		t.Fatalf("stdio args = %#v", cfg.Args)
	}
	if !reflect.DeepEqual(cfg.Env, map[string]string{"TOKEN": "$MCP_TOKEN"}) {
		t.Fatalf("stdio env = %#v", cfg.Env)
	}
	if cfg.OAuth || cfg.OAuthCallbackPort != 0 {
		t.Fatalf("hidden OAuth fields leaked into stdio config: %#v", cfg)
	}

	dialog.transport = config.MCPHttp
	dialog.oauth = true
	dialog.setInputValue(mcpFieldURL, "https://example.com/mcp")
	dialog.setInputValue(mcpFieldHeaders, "Authorization=Bearer $MCP_TOKEN")
	dialog.setInputValue(mcpFieldOAuthClientID, "client")
	dialog.setInputValue(mcpFieldOAuthClientSecret, "$MCP_SECRET")
	dialog.setInputValue(mcpFieldOAuthCallbackPort, "8765")

	_, cfg, err = dialog.formConfig()
	if err != nil {
		t.Fatalf("HTTP config error: %v", err)
	}
	if cfg.Command != "" || cfg.URL != "https://example.com/mcp" || !cfg.OAuth || cfg.OAuthCallbackPort != 8765 {
		t.Fatalf("HTTP config = %#v", cfg)
	}
	if !reflect.DeepEqual(cfg.Headers, map[string]string{"Authorization": "Bearer $MCP_TOKEN"}) {
		t.Fatalf("HTTP headers = %#v", cfg.Headers)
	}
}

func TestMCPServersTextFieldsAcceptSpaces(t *testing.T) {
	dialog, _, _ := newMCPServersTestDialog(t, nil)
	dialog.openForm("", config.MCPConfig{Type: config.MCPStdio})
	dialog.setInputValue(mcpFieldArgs, "one")
	dialog.setFormFocus(mcpFieldArgs)
	dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	if got := dialog.inputs[mcpFieldArgs].Value(); got != "one " {
		t.Fatalf("arguments after space = %q", got)
	}
}

func TestMCPServersPastesIntoFocusedTextField(t *testing.T) {
	dialog, _, _ := newMCPServersTestDialog(t, nil)
	dialog.openForm("", config.MCPConfig{Type: config.MCPHttp})
	dialog.setInputValue(mcpFieldURL, "https://")
	dialog.setFormFocus(mcpFieldURL)

	action := dialog.HandleMsg(tea.PasteMsg{Content: "example.com/mcp?token=a b"})
	if _, ok := action.(ActionCmd); !ok {
		t.Fatalf("paste action = %T, want ActionCmd", action)
	}
	if got := dialog.inputs[mcpFieldURL].Value(); got != "https://example.com/mcp?token=a b" {
		t.Fatalf("URL after paste = %q", got)
	}
}

func TestMCPServersRejectsPasteThatWouldExceedFieldLimit(t *testing.T) {
	dialog, _, _ := newMCPServersTestDialog(t, nil)
	dialog.openForm("", config.MCPConfig{Type: config.MCPStdio})
	dialog.setInputValue(mcpFieldArgs, "prefix")
	dialog.setFormFocus(mcpFieldArgs)

	action, ok := dialog.HandleMsg(tea.PasteMsg{Content: strings.Repeat("x", maxMCPFieldPasteBytes)}).(ActionCmd)
	if !ok || action.Cmd == nil {
		t.Fatalf("oversized paste action = %#v", action)
	}
	warning, ok := action.Cmd().(util.InfoMsg)
	if !ok || warning.Type != util.InfoTypeWarn || !strings.Contains(warning.Msg, "max 64KB") {
		t.Fatalf("oversized paste warning = %#v", warning)
	}
	if got := dialog.inputs[mcpFieldArgs].Value(); got != "prefix" {
		t.Fatalf("field changed after oversized paste: %q", got)
	}

	content := strings.Repeat("x", maxMCPFieldPasteBytes-len("prefix"))
	dialog.HandleMsg(tea.PasteMsg{Content: content})
	if got := len(dialog.inputs[mcpFieldArgs].Value()); got != maxMCPFieldPasteBytes {
		t.Fatalf("boundary paste length = %d, want %d", got, maxMCPFieldPasteBytes)
	}
}

func TestMCPServersPasteIgnoresNonEditableRows(t *testing.T) {
	dialog, _, _ := newMCPServersTestDialog(t, nil)
	dialog.openForm("existing", config.MCPConfig{Type: config.MCPStdio, Command: "npx"})
	dialog.setFormFocus(mcpFieldName)
	if action := dialog.HandleMsg(tea.PasteMsg{Content: "replacement"}); action != nil {
		t.Fatalf("immutable name paste action = %#v", action)
	}
	if got := dialog.inputs[mcpFieldName].Value(); got != "existing" {
		t.Fatalf("immutable name after paste = %q", got)
	}

	dialog.setFormFocus(mcpFieldType)
	if action := dialog.HandleMsg(tea.PasteMsg{Content: "http"}); action != nil {
		t.Fatalf("choice row paste action = %#v", action)
	}
	if dialog.transport != config.MCPStdio {
		t.Fatalf("transport changed to %q after paste", dialog.transport)
	}
}

func TestMCPServersFormRejectsInvalidInputs(t *testing.T) {
	dialog, _, _ := newMCPServersTestDialog(t, nil)
	dialog.openForm("", config.MCPConfig{Type: config.MCPStdio})

	for _, test := range []struct {
		name      string
		server    string
		transport config.MCPType
		command   string
		url       string
		timeout   string
		want      string
	}{
		{name: "invalid name", server: "bad name", transport: config.MCPStdio, command: "npx", timeout: "10", want: "name must contain"},
		{name: "missing command", server: "server", transport: config.MCPStdio, timeout: "10", want: "command is required"},
		{name: "relative URL", server: "server", transport: config.MCPHttp, url: "/mcp", timeout: "10", want: "absolute http or https URL"},
		{name: "negative timeout", server: "server", transport: config.MCPStdio, command: "npx", timeout: "-1", want: "non-negative integer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dialog.editingName = ""
			dialog.transport = test.transport
			dialog.setInputValue(mcpFieldName, test.server)
			dialog.setInputValue(mcpFieldCommand, test.command)
			dialog.setInputValue(mcpFieldURL, test.url)
			dialog.setInputValue(mcpFieldTimeout, test.timeout)
			_, _, err := dialog.formConfig()
			if err == nil || !containsError(err, test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestMCPServersSaveAndRemoveRunAsynchronously(t *testing.T) {
	dialog, _, ws := newMCPServersTestDialog(t, nil)
	dialog.openForm("", config.MCPConfig{Type: config.MCPStdio, Timeout: 10})
	dialog.setInputValue(mcpFieldName, "filesystem")
	dialog.setInputValue(mcpFieldCommand, "npx")
	ws.stateAfterSet = map[string]mcptools.ClientInfo{
		"filesystem": {Name: "filesystem", State: mcptools.StateConnected, Counts: mcptools.Counts{Tools: 3, Prompts: 1}},
	}

	action, ok := dialog.save().(ActionCmd)
	if !ok || action.Cmd == nil {
		t.Fatalf("save action = %#v", action)
	}
	if len(ws.setFields) != 0 {
		t.Fatal("save performed workspace I/O synchronously")
	}
	result := action.Cmd()
	if _, ok := ws.setFields["mcp.filesystem"]; !ok {
		t.Fatalf("saved fields = %#v", ws.setFields)
	}
	dialog.HandleMsg(result)
	if dialog.mode != mcpServersListMode || dialog.status != "Connected filesystem · 3 tools · 1 prompts" {
		t.Fatalf("save result mode/status = %v, %q", dialog.mode, dialog.status)
	}

	action = ActionCmd{Cmd: dialog.removeCmd("filesystem")}
	if len(ws.removed) != 0 {
		t.Fatal("remove performed workspace I/O synchronously")
	}
	result = action.Cmd()
	if !reflect.DeepEqual(ws.removed, []string{"mcp.filesystem"}) {
		t.Fatalf("removed fields = %#v", ws.removed)
	}
	dialog.HandleMsg(result)
	if _, ok := dialog.servers["filesystem"]; ok {
		t.Fatal("removed server remained in dialog")
	}
}

func TestWaitForMCPValidationReportsLiveStates(t *testing.T) {
	failure := errors.New("connection refused")
	for _, test := range []struct {
		name    string
		cfg     config.MCPConfig
		info    mcptools.ClientInfo
		wantErr string
	}{
		{name: "connected", info: mcptools.ClientInfo{Name: "server", State: mcptools.StateConnected}},
		{name: "needs auth", info: mcptools.ClientInfo{Name: "server", State: mcptools.StateNeedsAuth}},
		{name: "disabled", cfg: config.MCPConfig{Disabled: true}},
		{name: "error", info: mcptools.ClientInfo{Name: "server", State: mcptools.StateError, Error: failure}, wantErr: failure.Error()},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := waitForMCPValidation(t.Context(), func() map[string]mcptools.ClientInfo {
				return map[string]mcptools.ClientInfo{"server": test.info}
			}, "server", test.cfg)
			if test.wantErr == "" && err != nil {
				t.Fatalf("validation error: %v", err)
			}
			if test.wantErr != "" && (err == nil || !containsError(err, test.wantErr)) {
				t.Fatalf("validation error = %v, want containing %q", err, test.wantErr)
			}
		})
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := waitForMCPValidation(ctx, func() map[string]mcptools.ClientInfo { return nil }, "server", config.MCPConfig{})
	if err == nil || !containsError(err, "timed out validating") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestCommandsIncludesMCPServersManager(t *testing.T) {
	theme := styles.ThemeForProvider("")
	ws := &mcpServersTestWorkspace{cfg: &config.Config{}}
	commands, err := NewCommands(&common.Common{Workspace: ws, Styles: &theme}, "", false, false, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range commands.defaultCommands() {
		if item.id != "mcp_servers" {
			continue
		}
		action, ok := item.action.(ActionOpenDialog)
		if !ok || action.DialogID != MCPServersID {
			t.Fatalf("MCP command action = %#v", item.action)
		}
		return
	}
	t.Fatal("MCP Servers command missing")
}

func containsError(err error, text string) bool {
	return err != nil && text != "" && strings.Contains(err.Error(), text)
}
