package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/agent/tools"
	mcptools "github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/session"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/chat"
	"github.com/example-git/crux/internal/ui/completions"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
)

// PreviewData is the editable document shared by HTTP and the native renderer.
// All initial values come from the dummy fixture; edits exist only in the page.
type PreviewSettings struct {
	Debug                       bool     `json:"debug"`
	InstructionMode             string   `json:"instructionMode"`
	DisabledInstructionSections []string `json:"disabledInstructionSections"`
	DisableAutoSummarize        bool     `json:"disableAutoSummarize"`
	SummarizationContextCap     int64    `json:"summarizationContextCap"`
	SummarizationMaxTokens      int64    `json:"summarizationMaxTokens"`
	SummarizationFastMode       bool     `json:"summarizationFastMode"`
	CodexCompactionV2           bool     `json:"codexCompactionV2"`
}
type PreviewData struct {
	Directory           string                                `json:"directory"`
	RemoteAddress       string                                `json:"remoteAddress"`
	Authority           *config.RemoteAuthority               `json:"authority,omitempty"`
	ForegroundWaitCount int                                   `json:"foregroundWaitCount"`
	Messages            []*message.Message                    `json:"messages"`
	CodebaseIndex       proto.CodebaseIndexStatus             `json:"codebaseIndex"`
	Instructions        []dialog.InstructionPreviewSection    `json:"instructions"`
	Permission          permission.PermissionRequest          `json:"permission"`
	Settings            PreviewSettings                       `json:"settings"`
	Session             session.Session                       `json:"session"`
	Provider            providerregistry.Surface              `json:"provider"`
	Models              []catalog.Model                       `json:"models"`
	Files               []SessionFile                         `json:"files"`
	Usage               *oauthusage.Usage                     `json:"usage"`
	MCP                 map[string]mcptools.ClientInfo        `json:"mcp"`
	LSP                 map[string]workspace.LSPClientInfo    `json:"lsp"`
	Sessions            []session.Session                     `json:"sessions"`
	Tasks               []managedtask.View                    `json:"tasks"`
	FileMentions        []completions.FileCompletionValue     `json:"fileMentions"`
	Resources           []completions.ResourceCompletionValue `json:"resources"`
	Commands            []completions.CommandCompletionValue  `json:"commands"`
	Examples            map[string]PreviewExampleData         `json:"examples"`
	ReadyPlaceholder    string                                `json:"readyPlaceholder"`
	WorkingPlaceholder  string                                `json:"workingPlaceholder"`
}
type PreviewExampleData struct {
	Tool    string              `json:"tool"`
	Params  any                 `json:"params"`
	Result  *message.ToolResult `json:"result"`
	Status  chat.ToolStatus     `json:"status"`
	Message *message.Message    `json:"message"`
}

func (p *Preview) defaultData() *PreviewData {
	p.initialize(PreviewModels[0])
	u := p.ui
	ws := u.com.Workspace.(*previewWorkspace)
	sessions, _ := ws.ListSessions(context.Background())
	d := &PreviewData{Session: *u.session, Provider: ws.surface, Models: append([]catalog.Model(nil), PreviewModels...), Files: u.sessionFiles, Usage: u.providerUsage, MCP: u.mcpStates, LSP: u.lspStates, Sessions: sessions, Tasks: previewTasks(), Examples: map[string]PreviewExampleData{}, ReadyPlaceholder: u.readyPlaceholder, WorkingPlaceholder: u.workingPlaceholder}
	d.Permission = permission.PermissionRequest{ID: "fixture-permission", SessionID: d.Session.ID, ToolName: tools.BashToolName, Description: "Run the local fixture checks", Action: "execute", Path: "/preview/crush", Params: tools.BashPermissionsParams{Command: "go test ./fixture -v"}}
	d.Settings.InstructionMode = "all"
	d.Settings.DisabledInstructionSections = []string{}
	d.CodebaseIndex = proto.CodebaseIndexStatus{Enabled: true, State: "indexing", Serving: true, ProjectRoot: "/preview/crush", DatabasePath: "/preview/crush/.crux/index.db", StoreDirectory: "/preview/index-store", ConfiguredDatabasePath: "/preview/crush/.crux/index.db", ConfiguredStoreDirectory: "/preview/index-store", SourceMode: "local fixture", CredentialStatus: "ready", Model: "dummy-embeddings", IncludePaths: []string{"internal/", "foundation/"}, ExcludePaths: []string{"node_modules/", "vendor/"}, FilesTotal: 1280, FilesProcessed: 864, ChunksCreated: 4320, FilesSkipped: 12, CurrentPath: "internal/ui/model/preview_registry.go", Stage: "embedding"}
	for _, name := range []string{"Overview", "Project context", "Tooling instructions", "Provider context", "Runtime settings", "Disabled section"} {
		d.Instructions = append(d.Instructions, dialog.InstructionPreviewSection{ID: name, Label: name, Content: "# " + name + "\n\nLocal dummy instruction text for rendering tests.\n\n" + previewReport(name), Toggleable: true, Disabled: name == "Disabled section"})
	}
	d.Provider.Instructions = &providerregistry.InstructionSurface{Default: "Dummy native instructions", SelectionDefault: "crux", Profiles: map[string]string{"native": "Dummy native instructions"}}
	d.Provider.Models = nil // Model definitions have one owner: data.models.
	for _, e := range PreviewExamples() {
		d.Examples[e.ID] = PreviewExampleData{e.tool, e.params, e.result, e.status, e.msg}
	}
	for _, name := range []string{"preview.go", "preview_examples.go", "preview_long_outputs.go", "preview_overlays.go", "sidebar.go", "header.go", "ui.go", "chat.go", "layout.go", "session.go", "README.md", "go.mod"} {
		d.FileMentions = append(d.FileMentions, completions.FileCompletionValue{Path: "internal/ui/model/" + name})
	}
	for i, name := range []string{"Renderer notes", "Session transcript", "Task output", "Provider catalog", "Layout measurements", "Preview report"} {
		d.Resources = append(d.Resources, completions.ResourceCompletionValue{MCPName: "dummy-devtools", URI: fmt.Sprintf("fixture://preview/%d", i), Title: name, MIMEType: "text/plain"})
	}
	for _, name := range []string{"models", "providers", "tasks", "sessions", "reasoning", "summarization", "notifications", "instructions", "help", "quit"} {
		d.Commands = append(d.Commands, completions.CommandCompletionValue{ID: name, Title: "/" + name})
	}
	modelJSON, _ := json.Marshal(d.Models)
	d.Models = nil
	json.Unmarshal(modelJSON, &d.Models)
	p.ui = nil
	p.itemsKey = ""
	return d
}
func PreviewFixtureData() *PreviewData { p, _ := NewPreview(); return p.data }

// applyPreviewJSON merges against concrete fixture values. Interface values keep
// their existing Go type, so message parts still reach native type switches.
func applyPreviewJSON(v reflect.Value, raw json.RawMessage, path string) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
			v.SetZero()
			return nil
		}
		return fmt.Errorf("%s cannot be null", path)
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		return applyPreviewJSON(v.Elem(), raw, path)
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			var x any
			if err := json.Unmarshal(raw, &x); err != nil {
				return err
			}
			if x != nil && !reflect.TypeOf(x).AssignableTo(v.Type()) {
				return fmt.Errorf("%s requires an existing typed fixture part", path)
			}
			v.Set(reflect.ValueOf(x))
			return nil
		}
		x := reflect.New(v.Elem().Type()).Elem()
		x.Set(v.Elem())
		if err := applyPreviewJSON(x, raw, path); err != nil {
			return err
		}
		v.Set(x)
		return nil
	}
	if v.CanAddr() {
		if _, ok := v.Addr().Interface().(json.Unmarshaler); ok {
			return json.Unmarshal(raw, v.Addr().Interface())
		}
	}
	switch v.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for key, value := range fields {
			found := false
			for i := 0; i < v.NumField(); i++ {
				f := v.Type().Field(i)
				name := strings.Split(f.Tag.Get("json"), ",")[0]
				if name == "-" || !v.Field(i).CanSet() {
					continue
				}
				if name == "" {
					name = f.Name
				}
				if key == name {
					found = true
					if err := applyPreviewJSON(v.Field(i), value, path+"/"+key); err != nil {
						return err
					}
					break
				}
			}
			if !found {
				return fmt.Errorf("unknown fixture path %s/%s", path, key)
			}
		}
	case reflect.Map:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		for key, value := range fields {
			k := reflect.ValueOf(key).Convert(v.Type().Key())
			old := v.MapIndex(k)
			x := reflect.New(v.Type().Elem()).Elem()
			if old.IsValid() {
				x.Set(old)
			}
			if err := applyPreviewJSON(x, value, path+"/"+key); err != nil {
				return err
			}
			v.SetMapIndex(k, x)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return json.Unmarshal(raw, v.Addr().Interface())
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil {
			return err
		}
		next := reflect.MakeSlice(v.Type(), len(rows), len(rows))
		for i, row := range rows {
			if i < v.Len() {
				next.Index(i).Set(v.Index(i))
			} else if v.Len() > 0 {
				next.Index(i).Set(v.Index(0))
			}
			if err := applyPreviewJSON(next.Index(i), row, fmt.Sprintf("%s/%d", path, i)); err != nil {
				return err
			}
		}
		v.Set(next)
	default:
		if err := json.Unmarshal(raw, v.Addr().Interface()); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}
