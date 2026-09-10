package model

// This file supplies fixture state to the production UI. It deliberately does
// not implement any rendering: Preview.Render ends at the ordinary UI.View.
// The embedded demo never calls UI.Init or agent commands.

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"maps"
	"reflect"
	"strings"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/foundation/catalog"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	mcptools "github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/history"
	"github.com/example-git/crux/internal/lsp"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

var PreviewModels = []catalog.Model{
	{ID: "dummy-coder", Name: "Dummy Coder", ContextWindow: 280000, DefaultMaxTokens: 16000, CanReason: true, ReasoningLevels: []string{"low", "medium", "high"}, DefaultReasoningEffort: "medium", SupportsImages: true},
	{ID: "dummy-fast", Name: "Dummy Fast", ContextWindow: 64000, DefaultMaxTokens: 8000, SupportsImages: false},
	{ID: "dummy-long", Name: "Dummy Long Context", ContextWindow: 1000000, DefaultMaxTokens: 32000, CanReason: true, ReasoningLevels: []string{"low", "medium", "high"}, DefaultReasoningEffort: "high", SupportsImages: true},
}

type PreviewOptions struct {
	ImportID      string          `json:"importId"`
	MessageNumber int             `json:"messageNumber"`
	Bottom        bool            `json:"bottom"`
	Instance      string          `json:"instance"`
	Data          json.RawMessage `json:"data,omitempty"`
	FocusItem     string          `json:"focusItem"`
	Modal         string          `json:"modal"`
	Popover       string          `json:"popover"`
	MenuRow       int             `json:"menuRow"`
	Example       string          `json:"example"`
	ResetRevision uint64          `json:"resetRevision"`
	Cols          int             `json:"cols"`
	Rows          int             `json:"rows"`
	Model         string          `json:"model"`
	Scenario      string          `json:"scenario"`
	Compact       bool            `json:"compact"`
	PlanExpanded  bool            `json:"planExpanded"`
	ToolsExpanded bool            `json:"toolsExpanded"`
	ToolsCompact  bool            `json:"toolsCompact"`
	Input         string          `json:"input"`
	Scroll        int             `json:"scroll"`
	Submitted     []string        `json:"submitted"`
	Click         *PreviewPoint   `json:"click,omitempty"`
	Hover         *PreviewPoint   `json:"hover,omitempty"`
	Drag          *PreviewDrag    `json:"drag,omitempty"`
	SidebarTicks  int             `json:"sidebarTicks,omitempty"`
}
type PreviewDrag struct {
	From PreviewPoint `json:"from"`
	To   PreviewPoint `json:"to"`
}
type PreviewPoint struct {
	X int `json:"x"`
	Y int `json:"y"`
}
type PreviewItem struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
}
type PreviewFrame struct {
	SchemaNote     string                     `json:"schemaNote,omitempty"`
	ScrollOffset   int                        `json:"scrollOffset"`
	StoredMessages []PreviewStoredMessage     `json:"storedMessages,omitempty"`
	Imported       bool                       `json:"imported"`
	Regions        map[string]image.Rectangle `json:"regions"`
	Items          []PreviewItem              `json:"items"`
	ExampleNote    string                     `json:"exampleNote"`
	Content        string                     `json:"content"`
	Renderer       string                     `json:"renderer"`
	Cols           int                        `json:"cols"`
	Rows           int                        `json:"rows"`
	Compact        bool                       `json:"compact"`
	SidebarColumns int                        `json:"sidebarColumns"`
	MessageCount   int                        `json:"messageCount"`
	Model          string                     `json:"model"`
}

// Unused workspace operations are deliberately absent: there is no real
// workspace behind this adapter. A future accidental dependency fails loudly.
type previewWorkspace struct {
	taskData   *PreviewData
	surfaces   []providerregistry.Surface
	workingDir string
	history    []history.File
	workspace.Workspace
	cfg      *config.Config
	model    workspace.AgentModel
	sessions []session.Session
	surface  providerregistry.Surface
}

func (w *previewWorkspace) Config() *config.Config { return w.cfg }
func (w *previewWorkspace) AcceptedAuthority() *config.RemoteAuthority {
	if w.taskData == nil {
		return nil
	}
	return w.taskData.Authority
}
func (w *previewWorkspace) WorkingDir() string {
	if w.taskData != nil && w.taskData.Directory != "" {
		return w.taskData.Directory
	}
	if w.workingDir != "" {
		return w.workingDir
	}
	return "/preview/crush"
}

func (w *previewWorkspace) RemoteAddress() string {
	if w.taskData != nil {
		return w.taskData.RemoteAddress
	}
	return ""
}
func (w *previewWorkspace) ProviderSurfaces() []providerregistry.Surface {
	if w.surfaces != nil {
		return w.surfaces
	}
	return []providerregistry.Surface{w.surface}
}
func (w *previewWorkspace) PermissionSkipRequests() bool                    { return false }
func (w *previewWorkspace) AgentIsReady() bool                              { return true }
func (w *previewWorkspace) AgentModel() workspace.AgentModel                { return w.model }
func (w *previewWorkspace) ProjectNeedsInitialization() (bool, error)       { return false, nil }
func (w *previewWorkspace) SetCurrentSession(context.Context, string) error { return nil }

type Preview struct {
	runtimeConfig   *config.Config
	runtimeSurfaces []providerregistry.Surface
	initialModel    string
	schemaNote      string
	base            *PreviewData
	imported        bool
	workingDir      string
	storedMessages  []PreviewStoredMessage
	data            *PreviewData
	dataKey         string
	ui              *UI
	key             string
	itemsKey        string
}

func NewPreview() (*Preview, error) {
	p := &Preview{}
	p.data = p.defaultData()
	p.base = clonePreviewValue(reflect.ValueOf(p.data)).Interface().(*PreviewData)
	return p, nil
}
func previewJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (p *Preview) initialize(model catalog.Model) {
	selected := config.SelectedModel{Provider: "dummy-preview", Model: model.ID, MaxTokens: model.DefaultMaxTokens, ReasoningEffort: model.DefaultReasoningEffort}
	if p.runtimeConfig != nil {
		configured := p.runtimeConfig.Models[config.SelectedModelTypeLarge]
		if configured.Model == model.ID && configured.Provider == p.data.Provider.ID {
			selected = configured
		}
	}
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("dummy-preview", config.ProviderConfig{ID: "dummy-preview", Name: "Dummy Provider (local fixture)", Models: PreviewModels})
	cfg := &config.Config{Providers: providers, Models: map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: selected, config.SelectedModelTypeSmall: {Provider: "dummy-preview", Model: "dummy-fast"}}, Options: &config.Options{TUI: &config.TUIOptions{}, DisableDefaultProviders: true}, MCP: config.MCPs{}}
	cfg.Agents = map[string]config.Agent{config.AgentCoder: {ID: config.AgentCoder, Model: config.SelectedModelTypeLarge}}
	cfg.BindPreviewProviders([]catalog.Provider{{ID: "dummy-preview", Name: "Dummy Provider (local fixture)", Models: PreviewModels}})
	cfg.MCP["dummy-devtools"] = config.MCPConfig{}
	ws := &previewWorkspace{workingDir: p.workingDir, cfg: cfg, model: workspace.AgentModel{CatalogModel: model, ModelCfg: selected}, surface: providerregistry.Surface{ID: "dummy-preview", Name: "Dummy Provider", Available: true, FlatRate: true, Models: PreviewModels, DefaultLargeModel: "dummy-coder", DefaultSmallModel: "dummy-fast", Brand: &providerregistry.Brand{Label: "Dummy Provider", ShortName: "DUMMYAI", Color: "#7FC4FF", GradientA: "#1B3B8B", GradientB: "#7FC4FF"}}}
	if p.data != nil {
		cfg.Options.Debug = p.data.Settings.Debug
		cfg.Options.InstructionMode = p.data.Settings.InstructionMode
		cfg.Options.DisabledInstructionSections = p.data.Settings.DisabledInstructionSections
		cfg.Options.DisableAutoSummarize = p.data.Settings.DisableAutoSummarize
		cfg.Options.SummarizationContextCap = p.data.Settings.SummarizationContextCap
		cfg.Options.SummarizationMaxTokens = p.data.Settings.SummarizationMaxTokens
		cfg.Options.SummarizationFastMode = p.data.Settings.SummarizationFastMode
		cfg.Options.CodexCompactionV2 = p.data.Settings.CodexCompactionV2
		ws.surface = p.data.Provider
		ws.surface.Models = p.data.Models
		ws.sessions = p.data.Sessions
		selected.Provider = ws.surface.ID
		ws.model.ModelCfg = selected
		cfg.Models[config.SelectedModelTypeLarge] = selected
		cfg.Providers = csync.NewMap[string, config.ProviderConfig]()
		cfg.Providers.Set(ws.surface.ID, config.ProviderConfig{ID: ws.surface.ID, Name: ws.surface.Name, Models: p.data.Models})
		cfg.BindPreviewProviders([]catalog.Provider{{ID: catalog.ProviderID(ws.surface.ID), Name: ws.surface.Name, Models: p.data.Models}})
	}
	if p.runtimeConfig != nil {
		runtime := *p.runtimeConfig
		runtime.Models = maps.Clone(p.runtimeConfig.Models)
		runtime.Agents = maps.Clone(p.runtimeConfig.Agents)
		options := *p.runtimeConfig.Options
		runtime.Options = &options
		runtime.Options.Debug = p.data.Settings.Debug
		runtime.Models[config.SelectedModelTypeLarge] = selected
		runtime.Options.InstructionMode = p.data.Settings.InstructionMode
		runtime.Options.DisabledInstructionSections = p.data.Settings.DisabledInstructionSections
		runtime.Options.DisableAutoSummarize = p.data.Settings.DisableAutoSummarize
		runtime.Options.SummarizationContextCap = p.data.Settings.SummarizationContextCap
		runtime.Options.SummarizationMaxTokens = p.data.Settings.SummarizationMaxTokens
		runtime.Options.SummarizationFastMode = p.data.Settings.SummarizationFastMode
		runtime.Options.CodexCompactionV2 = p.data.Settings.CodexCompactionV2
		ws.cfg = &runtime
		ws.surfaces = append([]providerregistry.Surface(nil), p.runtimeSurfaces...)
		for i := range ws.surfaces {
			if ws.surfaces[i].ID == p.data.Provider.ID {
				ws.surfaces[i] = p.data.Provider.Clone()
				ws.surfaces[i].Models = p.data.Models
			}
		}
	}
	u := New(common.DefaultCommon(ws), "", false, "")
	u.width, u.height = 160, 60
	u.session = &session.Session{ID: "preview-session", Title: "Audit Visual Rendering for Inconsistencies", PromptTokens: 47400, CreatedAt: 1788690000, UpdatedAt: 1788690060}
	for i, content := range []string{"Trace and reproduce task-notification delivery failures", "Fixing lifecycle gaps and adding regression coverage", "Validate notification recovery and concurrency behavior"} {
		status := session.TodoStatusPending
		if i == 0 {
			status = session.TodoStatusCompleted
		}
		if i == 1 {
			status = session.TodoStatusInProgress
		}
		u.session.Todos = append(u.session.Todos, session.Todo{Content: content, ActiveForm: content, Status: status})
	}
	files := []struct {
		path     string
		add, del int
	}{{"internal/agent/agent.go", 6, 0}, {"internal/app/task_delivery_test.go", 45, 0}, {"internal/agent/notification_delivery_test.go", 70, 0}, {"internal/app/tasks.go", 32, 20}}
	for _, file := range files {
		u.sessionFiles = append(u.sessionFiles, SessionFile{FirstVersion: history.File{Path: "/preview/crush/" + file.path, Exists: true}, LatestVersion: history.File{Path: "/preview/crush/" + file.path, Exists: true}, Additions: file.add, Deletions: file.del})
	}
	u.providerUsage = &oauthusage.Usage{ProviderID: "dummy-preview", Windows: []oauthusage.Window{{Name: "weekly", Percent: 54}}}
	u.mcpStates = map[string]mcptools.ClientInfo{"dummy-devtools": {Name: "dummy-devtools", State: mcptools.StateConnected, Counts: mcptools.Counts{Tools: 29}}}
	u.lspStates = map[string]workspace.LSPClientInfo{"dummy-gopls": {Name: "dummy-gopls", State: lsp.StateReady}}
	u.skillStates = nil // The production sidebar also displays embedded builtin skills.
	u.readyPlaceholder = "Ready for instructions"
	u.workingPlaceholder = "Thinking..."
	if p.data != nil {
		sessionCopy := p.data.Session
		sessionCopy.Todos = append([]session.Todo(nil), p.data.Session.Todos...)
		u.session = &sessionCopy
		u.sessionFiles = p.data.Files
		u.providerUsage = p.data.Usage
		u.mcpStates = p.data.MCP
		u.lspStates = p.data.LSP
		u.readyPlaceholder = p.data.ReadyPlaceholder
		u.workingPlaceholder = p.data.WorkingPlaceholder
	}
	u.pillsExpanded = true
	u.setState(uiChat, uiFocusEditor)
	p.ui = u
	p.itemsKey = ""
}

func (p *Preview) Render(o PreviewOptions) (PreviewFrame, error) {
	if o.SidebarTicks < 0 || o.SidebarTicks > 1000 {
		return PreviewFrame{}, fmt.Errorf("sidebar ticks must be between 0 and 1000")
	}
	if o.Example == "" {
		o.Example = "all"
	}
	if o.MessageNumber < 0 {
		return PreviewFrame{}, fmt.Errorf("message number cannot be negative")
	}
	dataKey := string(o.Data)
	if dataKey != p.dataKey {
		fresh := &Preview{data: clonePreviewValue(reflect.ValueOf(p.base)).Interface().(*PreviewData)}
		if len(o.Data) > 0 {
			if !strings.HasPrefix(strings.TrimSpace(string(o.Data)), "{") {
				return PreviewFrame{}, fmt.Errorf("data must be a fixture object")
			}
			if err := applyPreviewJSON(reflect.ValueOf(fresh.data).Elem(), o.Data, ""); err != nil {
				return PreviewFrame{}, err
			}
		}
		if len(fresh.data.Provider.Models) > 0 {
			return PreviewFrame{}, fmt.Errorf("edit models at /models, not /provider/models")
		}
		if fresh.data.Session.MessageCount != p.data.Session.MessageCount {
			return PreviewFrame{}, fmt.Errorf("session/MessageCount is derived from transcript items")
		}
		for id := range fresh.data.Examples {
			if _, ok := p.data.Examples[id]; !ok {
				return PreviewFrame{}, fmt.Errorf("unknown fixture example data %q", id)
			}
		}
		p.data = fresh.data
		p.dataKey = dataKey
		p.ui = nil
		p.key = ""
	}

	if !previewMenuValid(o.Modal, PreviewModals) || !previewMenuValid(o.Popover, PreviewPopovers) || o.MenuRow < 0 || o.MenuRow > 100 {
		return PreviewFrame{}, fmt.Errorf("invalid preview menu selection")
	}
	if o.Example != "" && o.Example != "all" {
		valid := false
		for _, e := range PreviewExamples() {
			if e.ID == o.Example {
				valid = true
				break
			}
		}
		if !valid {
			return PreviewFrame{}, fmt.Errorf("unknown fixture example %q", o.Example)
		}
	}

	if o.Cols < 45 || o.Cols > 500 || o.Rows < 15 || o.Rows > 200 {
		return PreviewFrame{}, fmt.Errorf("preview dimensions must be 45–500 columns and 15–200 rows")
	}
	if o.Scenario != "working" && o.Scenario != "idle" && o.Scenario != "error" {
		return PreviewFrame{}, fmt.Errorf("unknown session scenario %q", o.Scenario)
	}
	modelID := o.Model
	if p.runtimeConfig != nil {
		provider, id, ok := strings.Cut(o.Model, "::")
		if !ok {
			return PreviewFrame{}, fmt.Errorf("session model must be provider-qualified; select a model from the session catalog")
		}
		modelID = id
		if provider != p.data.Provider.ID {
			surface, found := providerregistry.LookupSurface(p.runtimeSurfaces, provider)
			if !found {
				return PreviewFrame{}, fmt.Errorf("unknown launch provider %q", provider)
			}
			p.data.Provider = surface.Clone()
			p.data.Models = p.data.Provider.Models
			p.data.Provider.Models = nil
		}
	}
	var selected *catalog.Model
	for i := range p.data.Models {
		if p.data.Models[i].ID == modelID {
			selected = &p.data.Models[i]
			break
		}
	}
	if selected == nil {
		return PreviewFrame{}, fmt.Errorf("unknown dummy model %q", o.Model)
	}
	if len(o.Input) > 16000 || len(o.Submitted) > 30 {
		return PreviewFrame{}, fmt.Errorf("preview input limit exceeded")
	}
	if p.ui == nil || p.key != o.Instance+":"+o.Model {
		p.initialize(*selected)
		p.key = o.Instance + ":" + o.Model
	}
	u := p.ui
	u.width, u.height = o.Cols, o.Rows
	u.forceCompactMode = o.Compact
	u.pillsExpanded = o.PlanExpanded
	u.agentBusyCache.set(o.Scenario == "working")
	u.foregroundWaitSessionID = u.session.ID
	u.foregroundWaitCount = p.data.ForegroundWaitCount
	for i := range u.session.Todos {
		if p.imported || len(o.Data) > 2 {
			break
		}
		status := session.TodoStatusPending
		if i == 0 || o.Scenario == "idle" {
			status = session.TodoStatusCompleted
		} else if i == 1 {
			status = session.TodoStatusInProgress
		}
		u.session.Todos[i].Status = status
	}
	u.textarea.Placeholder = u.readyPlaceholder
	if o.Scenario == "working" {
		u.textarea.Placeholder = u.workingPlaceholder
	}
	u.textarea.SetValue(o.Input)
	u.textarea.MoveToEnd()
	itemsKey := previewJSON([]any{o.Example, p.dataKey, o.ResetRevision, o.Model, o.Scenario, o.ToolsExpanded, o.ToolsCompact, o.Submitted})
	if p.itemsKey != itemsKey {
		u.chat.SetMessages(p.exampleItems(o)...)
		p.itemsKey = itemsKey
	}
	u.session.MessageCount = int64(u.chat.Len())
	u.updateLayoutAndSize()
	u.chat.ScrollToTop()
	if o.FocusItem != "" {
		index, ok := u.chat.idInxMap[o.FocusItem]
		if !ok {
			return PreviewFrame{}, fmt.Errorf("unknown preview item %q", o.FocusItem)
		}
		u.chat.ScrollToIndex(index)
	}
	if o.MessageNumber > 0 {
		target := ""
		if p.imported {
			if o.MessageNumber > len(p.storedMessages) {
				return PreviewFrame{}, fmt.Errorf("message number exceeds %d stored messages", len(p.storedMessages))
			}
			target = p.storedMessages[o.MessageNumber-1].ItemID
		} else {
			for id, index := range u.chat.idInxMap {
				if index == o.MessageNumber-1 {
					target = id
					break
				}
			}
		}
		if target == "" {
			return PreviewFrame{}, fmt.Errorf("message %d has no standalone rendered item", o.MessageNumber)
		}
		u.chat.ScrollToIndex(u.chat.idInxMap[target])
	}
	if o.Bottom {
		u.chat.ScrollToBottom()
	} else {
		u.chat.ScrollBy(o.Scroll)
	}
	u.status.ClearInfoMsg()
	if len(o.Submitted) > 0 {
		u.status.SetInfoMsg(util.InfoMsg{Type: util.InfoTypeInfo, Msg: "Dummy session: input recorded locally"})
	}
	panelClick := o.Click
	if o.Modal != "none" && o.Modal != "" || o.Popover != "none" && o.Popover != "" {
		o.Click = nil
	}
	if o.Click != nil && image.Pt(o.Click.X, o.Click.Y).In(u.layout.sidebar) {
		u.updateSidebarScrollState()
		u.Update(tea.MouseClickMsg(tea.Mouse{X: o.Click.X, Y: o.Click.Y, Button: uv.MouseLeft}))
	}
	if o.Click != nil && image.Pt(o.Click.X, o.Click.Y).In(u.layout.main) {
		x, y := o.Click.X-u.layout.main.Min.X, o.Click.Y-u.layout.main.Min.Y
		if handled, cmd := u.chat.HandleMouseDown(x, y); handled {
			u.chat.HandleMouseUp(x, y)
			if cmd != nil {
				if delayed, ok := cmd().(DelayedClickMsg); ok {
					u.chat.HandleDelayedClick(delayed)
				}
			}
		}
	}
	if err := p.applyPreviewOverlays(o); err != nil {
		return PreviewFrame{}, err
	}
	view := u.View()
	if o.Hover != nil {
		u.Update(tea.MouseMotionMsg{X: o.Hover.X, Y: o.Hover.Y, Button: uv.MouseNone})
		view = u.View()
	}
	// Drive production updates deterministically without running async fixture
	// commands or copying fixture selections to the system clipboard.
	if o.SidebarTicks > 0 {
		u.syncSidebarDirectoryTicker()
		for range o.SidebarTicks {
			u.Update(sidebarDirectoryTickMsg{u.sidebarSession.tickGeneration})
		}
		view = u.View()
	}
	if o.Drag != nil {
		u.Update(tea.MouseClickMsg{X: o.Drag.From.X, Y: o.Drag.From.Y, Button: uv.MouseLeft})
		u.Update(tea.MouseMotionMsg{X: o.Drag.To.X, Y: o.Drag.To.Y, Button: uv.MouseLeft})
		u.Update(tea.MouseReleaseMsg{X: o.Drag.To.X, Y: o.Drag.To.Y, Button: uv.MouseLeft})
		view = u.View()
	}
	if panelClick != nil && u.taskPanel != nil {
		p.clickTaskPanel(*panelClick, o.Modal == "tasks")
		view = u.View()
	}
	return p.frame(o, view.Content), nil
}
func (p *Preview) frame(o PreviewOptions, content string) PreviewFrame {
	items := make([]PreviewItem, p.ui.chat.Len())
	for id, index := range p.ui.chat.idInxMap {
		items[index] = PreviewItem{ID: id, Index: index}
	}
	layout := p.ui.layout
	regions := map[string]image.Rectangle{"header": layout.header, "chat": layout.main, "todos": layout.pills, "editor": layout.editor, "sidebar": layout.sidebar, "status": layout.status}
	return PreviewFrame{SchemaNote: p.schemaNote, ScrollOffset: p.ui.chat.list.Offset(), Imported: p.imported, StoredMessages: p.storedMessages, Regions: regions, Items: items, ExampleNote: p.previewNote(o), Content: content, Renderer: "crux/internal/ui/model.UI.View", Cols: o.Cols, Rows: o.Rows, Compact: p.ui.isCompact, SidebarColumns: p.ui.layout.sidebar.Dx(), MessageCount: p.ui.chat.Len(), Model: o.Model}
}

func (p *Preview) previewNote(o PreviewOptions) string {
	if p.imported && o.Modal == "none" && o.Popover == "none" {
		return fmt.Sprintf("Read-only session snapshot · %d stored messages · no agents or tools are running", len(p.data.Messages))
	}
	return previewOverlayNote(o)
}
