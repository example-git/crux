package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/agent"
	mcptools "github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/codebaseindex"
	"github.com/example-git/crux/internal/commands"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/history"
	"github.com/example-git/crux/internal/lsp"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/projects"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/shell"
	"github.com/example-git/crux/internal/skills"
	managedtask "github.com/example-git/crux/internal/task"
)

// AppWorkspace implements the Workspace interface by delegating
// directly to an in-process [app.App] instance. This is the default
// mode when the client/server architecture is not enabled.
type AppWorkspace struct {
	app   *app.App
	store *config.ConfigStore
}

// NewAppWorkspace creates a new AppWorkspace wrapping the given app
// and config store.
func NewAppWorkspace(a *app.App, store *config.ConfigStore) *AppWorkspace {
	return &AppWorkspace{
		app:   a,
		store: store,
	}
}

// -- Sessions --

func (w *AppWorkspace) CreateSession(ctx context.Context, title string) (session.Session, error) {
	return w.app.Sessions.Create(ctx, title)
}

func (w *AppWorkspace) GetSession(ctx context.Context, sessionID string) (session.Session, error) {
	return w.app.Sessions.Get(ctx, sessionID)
}

func (w *AppWorkspace) ListSessions(ctx context.Context) ([]session.Session, error) {
	return w.app.Sessions.List(ctx)
}

func (w *AppWorkspace) SaveSession(ctx context.Context, sess session.Session) (session.Session, error) {
	return w.app.Sessions.Save(ctx, sess)
}

func (w *AppWorkspace) SetSessionMode(ctx context.Context, sessionID string, mode session.Mode) (session.Session, error) {
	if err := w.app.Sessions.SetMode(ctx, sessionID, mode); err != nil {
		return session.Session{}, err
	}
	return w.app.Sessions.Get(ctx, sessionID)
}

func (w *AppWorkspace) DeleteSession(ctx context.Context, sessionID string) error {
	if err := w.app.Sessions.Delete(ctx, sessionID); err != nil {
		return err
	}
	w.app.ResetAgentSession(sessionID)
	return nil
}

func (w *AppWorkspace) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return w.app.Sessions.CreateAgentToolSessionID(messageID, toolCallID)
}

func (w *AppWorkspace) ParseAgentToolSessionID(sessionID string) (string, string, bool) {
	return w.app.Sessions.ParseAgentToolSessionID(sessionID)
}

// SetCurrentSession reports the active session to herdr so the pane
// can persist a resumable reference. Multi-client presence tracking
// is irrelevant in single-client local mode, but herdr still needs
// to know which session is live to support agent resume.
func (w *AppWorkspace) SetCurrentSession(ctx context.Context, sessionID string) error {
	w.app.ReportCurrentSession(sessionID)
	return nil
}

// -- Messages --

func (w *AppWorkspace) ListMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	// Drain any debounced updates so the caller observes the latest
	// in-memory state. message.Service buffers streaming deltas and a
	// cold List would otherwise miss them at session-switch time.
	if err := w.app.Messages.FlushAll(ctx); err != nil {
		return nil, err
	}
	return w.app.Messages.List(ctx, sessionID)
}

func (w *AppWorkspace) ListUserMessages(ctx context.Context, sessionID string) ([]message.Message, error) {
	return w.app.Messages.ListUserMessages(ctx, sessionID)
}

func (w *AppWorkspace) ListAllUserMessages(ctx context.Context) ([]message.Message, error) {
	return w.app.Messages.ListAllUserMessages(ctx)
}

// -- Agent --

func (w *AppWorkspace) AgentRun(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) error {
	coordinator := w.app.CurrentAgentCoordinator()
	if coordinator == nil {
		return errors.New("agent coordinator not initialized")
	}
	_, err := coordinator.Run(ctx, sessionID, prompt, attachments...)
	return err
}

func (w *AppWorkspace) AgentRunShellCommand(ctx context.Context, sessionID, command string, termWidth int, onProgress func(string), isFirstMessage bool) (proto.ShellCommandResponse, error) {
	var persist shell.PersistFunc
	if sessionID != "" {
		persist = func(cmd, output string, exitCode int) error {
			return shell.PersistOutput(ctx, w.app.Messages, sessionID, cmd, output, exitCode)
		}
	}

	opts := shell.RunOptions{
		Command:   command,
		Cwd:       w.store.WorkingDir(),
		TermWidth: termWidth,
	}

	var result shell.CaptureResult
	var err error

	if onProgress != nil {
		result, err = shell.RunAndCaptureStream(ctx, opts, onProgress)
	} else {
		result, err = shell.RunAndPersist(ctx, opts, persist)
	}

	if err != nil && onProgress == nil {
		return proto.ShellCommandResponse{}, err
	}

	// Persist if we used the streaming path (persist wasn't called by RunAndPersist).
	if onProgress != nil && persist != nil {
		if persistErr := persist(command, result.Output, result.ExitCode); persistErr != nil {
			slog.Error("Failed to persist shell command output", "error", persistErr, "command", command)
		}
	}

	// Generate a title from the shell command if it was the first message.
	if coordinator := w.app.CurrentAgentCoordinator(); isFirstMessage && coordinator != nil {
		titleCtx := context.WithoutCancel(ctx)
		coordinator.GenerateTitle(titleCtx, sessionID, "$ "+command)
	}

	return proto.ShellCommandResponse{
		Output:   result.Output,
		ExitCode: result.ExitCode,
	}, nil
}

func (w *AppWorkspace) AgentCancel(sessionID string) {
	if coordinator := w.app.CurrentAgentCoordinator(); coordinator != nil {
		coordinator.Cancel(sessionID)
	}
}

func (w *AppWorkspace) AgentIsBusy() bool {
	coordinator := w.app.CurrentAgentCoordinator()
	if coordinator == nil {
		return false
	}
	return coordinator.IsBusy()
}

func (w *AppWorkspace) AgentIsSessionBusy(sessionID string) bool {
	coordinator := w.app.CurrentAgentCoordinator()
	if coordinator == nil {
		return false
	}
	return coordinator.IsSessionBusy(sessionID)
}

func (w *AppWorkspace) AgentModel() AgentModel {
	coordinator := w.app.CurrentAgentCoordinator()
	if coordinator == nil {
		return AgentModel{}
	}
	m := coordinator.Model()
	return AgentModel{
		CatalogModel: m.CatalogModel,
		ModelCfg:     m.ModelCfg,
	}
}

func (w *AppWorkspace) AgentInstructionSnapshot(ctx context.Context) (agent.InstructionSnapshot, error) {
	coordinator := w.app.CurrentAgentCoordinator()
	if coordinator == nil {
		return agent.InstructionSnapshot{}, ErrAgentNotInitialized
	}
	return agent.CurrentInstructionSnapshot(ctx, coordinator)
}

func (w *AppWorkspace) AgentIsReady() bool {
	return w.app.CurrentAgentCoordinator() != nil
}

func (w *AppWorkspace) AgentReadyErr() error {
	if w.app.CurrentAgentCoordinator() == nil {
		return ErrAgentNotInitialized
	}
	return nil
}

func (w *AppWorkspace) AgentQueuedPrompts(sessionID string) int {
	coordinator := w.app.CurrentAgentCoordinator()
	if coordinator == nil {
		return 0
	}
	return coordinator.QueuedPrompts(sessionID)
}

func (w *AppWorkspace) AgentQueuedPromptsList(sessionID string) []agent.QueuedPrompt {
	coordinator := w.app.CurrentAgentCoordinator()
	if coordinator == nil {
		return nil
	}
	return coordinator.QueuedPromptsList(sessionID)
}

func (w *AppWorkspace) AgentClearQueue(sessionID string) {
	if coordinator := w.app.CurrentAgentCoordinator(); coordinator != nil {
		coordinator.ClearQueue(sessionID)
	}
}

func (w *AppWorkspace) AgentDetachForegroundJobs() int {
	return w.app.BackgroundShells.DetachForeground()
}

func (w *AppWorkspace) AgentSummarize(ctx context.Context, sessionID string) error {
	coordinator := w.app.CurrentAgentCoordinator()
	if coordinator == nil {
		return errors.New("agent coordinator not initialized")
	}
	return coordinator.Summarize(ctx, sessionID)
}

func (w *AppWorkspace) SessionRewind(ctx context.Context, sessionID, messageID string, summarize, restoreFiles bool) error {
	return w.app.RewindSession(ctx, sessionID, messageID, summarize, restoreFiles)
}

func (w *AppWorkspace) AgentSuggestPrompt(ctx context.Context, sessionID string) (string, error) {
	coordinator := w.app.CurrentAgentCoordinator()
	if coordinator == nil {
		return "", errors.New("agent coordinator not initialized")
	}
	return coordinator.SuggestPrompt(ctx, sessionID)
}

func (w *AppWorkspace) UpdateAgentModel(ctx context.Context, expected config.AgentModelState) error {
	return w.app.UpdateAgentModel(ctx, expected)
}

func (w *AppWorkspace) CreateAgentDefinition(_ context.Context, request proto.CreateAgentDefinitionRequest) (string, error) {
	return agent.CreateAgentDefinition(w.WorkingDir(), w.Config(), agentDefinitionTemplate(request))
}

func agentDefinitionTemplate(request proto.CreateAgentDefinitionRequest) agent.AgentDefinitionTemplate {
	template := agent.AgentDefinitionTemplate{
		Scope:       agent.AgentDefinitionScope(request.Scope),
		Name:        request.Name,
		Description: request.Description,
		Model:       request.Model,
		Tools:       request.Tools,
	}
	if request.Script != nil {
		template.Script = &agent.AgentDefinitionScriptTemplate{
			Path:      request.Script.Path,
			Timeout:   request.Script.Timeout,
			Variables: make(map[string]agent.AgentDefinitionScriptVariableTemplate, len(request.Script.Variables)),
		}
		for name, variable := range request.Script.Variables {
			template.Script.Variables[name] = agent.AgentDefinitionScriptVariableTemplate{
				Flag:     variable.Flag,
				Required: variable.Required,
				Default:  variable.Default,
				Value:    variable.Value,
				Values:   variable.Values,
			}
		}
	}
	return template
}

func (w *AppWorkspace) InitCoderAgent(ctx context.Context) error {
	return w.app.InitCoderAgent(ctx)
}

func (w *AppWorkspace) InitCoderAgentNonInteractive(ctx context.Context) error {
	return w.app.InitCoderAgentNonInteractive(ctx)
}

func (w *AppWorkspace) GetDefaultSmallModel(providerID string) (config.SelectedModel, error) {
	return w.app.GetDefaultSmallModel(providerID)
}

// -- Tasks --

func (w *AppWorkspace) ListTasks(ctx context.Context) ([]managedtask.View, error) {
	return w.app.ListTasks(ctx)
}

func (w *AppWorkspace) TaskOutput(ctx context.Context, id string, wait bool, timeout time.Duration) (managedtask.OutputResult, error) {
	return w.app.TaskOutput(ctx, id, wait, timeout)
}

func (w *AppWorkspace) StopTask(ctx context.Context, id string) (managedtask.View, error) {
	return w.app.StopTask(ctx, id)
}

func (w *AppWorkspace) ContinueTask(ctx context.Context, id, parentSessionID, prompt string) (managedtask.View, error) {
	return w.app.ContinueTask(ctx, id, parentSessionID, prompt)
}

func (w *AppWorkspace) ListTaskNotifications(ctx context.Context, parentSessionID string, unreadOnly bool) ([]managedtask.Notification, error) {
	return w.app.ListTaskNotifications(ctx, parentSessionID, unreadOnly)
}

func (w *AppWorkspace) MarkTaskNotificationRead(ctx context.Context, notificationID string) (managedtask.Notification, error) {
	return w.app.MarkTaskNotificationRead(ctx, notificationID)
}

// -- Permissions --

func (w *AppWorkspace) PermissionGrant(perm permission.PermissionRequest) bool {
	return w.app.Permissions.Grant(perm)
}

func (w *AppWorkspace) PermissionGrantPersistent(perm permission.PermissionRequest) bool {
	return w.app.Permissions.GrantPersistent(perm)
}

func (w *AppWorkspace) PermissionDeny(perm permission.PermissionRequest) bool {
	return w.app.Permissions.Deny(perm)
}

func (w *AppWorkspace) PermissionSkipRequests() bool {
	return w.app.Permissions.SkipRequests()
}

func (w *AppWorkspace) PermissionSetSkipRequests(skip bool) {
	w.app.Permissions.SetSkipRequests(skip)
}

// -- Questions --

func (w *AppWorkspace) QuestionAnswer(responses []question.Answer) bool {
	return w.app.Questions.Answer(responses)
}

func (w *AppWorkspace) QuestionCancel() bool {
	return w.app.Questions.Cancel()
}

// -- FileTracker --

func (w *AppWorkspace) FileTrackerRecordRead(ctx context.Context, sessionID, path string) {
	w.app.FileTracker.RecordRead(ctx, sessionID, path)
}

func (w *AppWorkspace) FileTrackerLastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return w.app.FileTracker.LastReadTime(ctx, sessionID, path)
}

func (w *AppWorkspace) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return w.app.FileTracker.ListReadFiles(ctx, sessionID)
}

// -- History --

func (w *AppWorkspace) ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error) {
	return w.app.History.ListLatestCheckpointFiles(ctx, sessionID)
}

// -- LSP --

func (w *AppWorkspace) LSPStart(ctx context.Context, path string) {
	w.app.LSPManager.Start(ctx, path)
}

func (w *AppWorkspace) LSPStopAll(ctx context.Context) {
	w.app.LSPManager.StopAll(ctx)
}

func (w *AppWorkspace) LSPGetStates() map[string]LSPClientInfo {
	states := app.GetLSPStates()
	result := make(map[string]LSPClientInfo, len(states))
	for k, v := range states {
		result[k] = LSPClientInfo{
			Name:            v.Name,
			State:           v.State,
			Error:           v.Error,
			DiagnosticCount: v.DiagnosticCount,
			ConnectedAt:     v.ConnectedAt,
		}
	}
	return result
}

func (w *AppWorkspace) LSPGetDiagnosticCounts(name string) lsp.DiagnosticCounts {
	state, ok := app.GetLSPState(name)
	if !ok || state.Client == nil {
		return lsp.DiagnosticCounts{}
	}
	return state.Client.GetDiagnosticCounts()
}

// -- Config (read-only) --

func (w *AppWorkspace) Config() *config.Config {
	return w.store.Config()
}

func (w *AppWorkspace) ProviderSurfaces() []providerregistry.Surface {
	surfaces := config.ProviderSurfaces(w.store.Config())
	for i := range surfaces {
		surfaces[i] = surfaces[i].Clone()
	}
	return surfaces
}

func (w *AppWorkspace) WorkingDir() string {
	return w.store.WorkingDir()
}

func (w *AppWorkspace) Resolver() config.VariableResolver {
	return w.store.Resolver()
}

// -- Config mutations --

func (w *AppWorkspace) UpdatePreferredModel(scope config.Scope, modelType config.SelectedModelType, model config.SelectedModel, owner providerregistry.RegistrationOwner) (config.AgentModelState, error) {
	return w.store.UpdatePreferredModelForOwner(scope, modelType, model, owner)
}

func (w *AppWorkspace) SetProviderDisabled(scope config.Scope, owner providerregistry.RegistrationOwner, disabled bool) error {
	if err := w.store.SetProviderDisabled(scope, owner, disabled); err != nil {
		return err
	}
	go mcptools.Reinitialize(context.Background(), w.store)
	return nil
}

func (w *AppWorkspace) SetCompactMode(scope config.Scope, enabled bool) error {
	return w.store.SetCompactMode(scope, enabled)
}

func (w *AppWorkspace) SetProviderAPIKey(scope config.Scope, providerID string, apiKey any) error {
	owner, err := config.ProviderCredentialOwner(providerID, apiKey)
	if err != nil {
		return err
	}
	if err := w.store.SetProviderAPIKey(scope, providerID, apiKey); err != nil {
		return err
	}
	w.store.SignalAuthComplete(owner)
	return nil
}

func (w *AppWorkspace) RemoveProviderCredentials(scope config.Scope, owner providerregistry.RegistrationOwner) error {
	return w.store.RemoveProviderCredentials(scope, owner)
}

func (w *AppWorkspace) SetConfigField(scope config.Scope, key string, value any) error {
	if err := w.store.SetConfigField(scope, key, value); err != nil {
		return err
	}
	if coordinator := w.app.CurrentAgentCoordinator(); skills.ConfigKeyAffectsDiscovery(key) && coordinator != nil {
		if err := coordinator.UpdateModels(context.Background()); err != nil {
			return fmt.Errorf("refresh agent after skill configuration change: %w", err)
		}
	}
	go mcptools.Reinitialize(context.Background(), w.store)
	return nil
}

func (w *AppWorkspace) RemoveConfigField(scope config.Scope, key string) error {
	if err := w.store.RemoveConfigField(scope, key); err != nil {
		return err
	}
	if coordinator := w.app.CurrentAgentCoordinator(); skills.ConfigKeyAffectsDiscovery(key) && coordinator != nil {
		if err := coordinator.UpdateModels(context.Background()); err != nil {
			return fmt.Errorf("refresh agent after skill configuration change: %w", err)
		}
	}
	go mcptools.Reinitialize(context.Background(), w.store)
	return nil
}

func (w *AppWorkspace) ImportCopilot() (*oauth.Token, bool) {
	return w.store.ImportCopilot()
}

func (w *AppWorkspace) RefreshOAuthToken(ctx context.Context, scope config.Scope, owner providerregistry.RegistrationOwner) error {
	_, err := w.store.RefreshOAuthTokenForOwner(ctx, scope, owner)
	return err
}

func (w *AppWorkspace) CodebaseIndexStatus(ctx context.Context) (proto.CodebaseIndexStatus, error) {
	coordinator := w.app.CurrentAgentCoordinator()
	status, err := agent.CodebaseIndexStatus(ctx, coordinator)
	if err != nil {
		return proto.CodebaseIndexStatus{}, err
	}
	result := codebaseIndexStatusProto(w.store.Config().Tools.CodebaseSearch, status)
	result.MemoryActivity = agent.AutoMemoryActivity(coordinator)
	return result, nil
}

func (w *AppWorkspace) UpdateCodebaseIndex(ctx context.Context, update proto.CodebaseIndexUpdate) (proto.CodebaseIndexStatus, error) {
	filters := codebaseindex.NormalizeProjectFilters(codebaseindex.ProjectFilters{
		IncludePaths: update.IncludePaths,
		ExcludePaths: update.ExcludePaths,
	})
	if err := w.store.SetConfigFields(config.ScopeWorkspace, map[string]any{
		"tools.codebase_search.enabled":         update.Enabled,
		"tools.codebase_search.database_path":   strings.TrimSpace(update.DatabasePath),
		"tools.codebase_search.store_directory": strings.TrimSpace(update.StoreDirectory),
		"tools.codebase_search.include_paths":   filters.IncludePaths,
		"tools.codebase_search.exclude_paths":   filters.ExcludePaths,
	}); err != nil {
		return proto.CodebaseIndexStatus{}, err
	}
	coordinator := w.app.CurrentAgentCoordinator()
	if coordinator == nil {
		return proto.CodebaseIndexStatus{}, ErrAgentNotInitialized
	}
	if err := coordinator.UpdateModels(ctx); err != nil {
		return proto.CodebaseIndexStatus{}, err
	}
	if update.Reindex {
		status, err := agent.ReconcileCodebaseIndex(ctx, coordinator)
		if err != nil {
			return proto.CodebaseIndexStatus{}, err
		}
		result := codebaseIndexStatusProto(w.store.Config().Tools.CodebaseSearch, status)
		result.MemoryActivity = agent.AutoMemoryActivity(coordinator)
		return result, nil
	}
	return w.CodebaseIndexStatus(ctx)
}

func codebaseIndexStatusProto(settings config.ToolCodebaseSearch, status codebaseindex.StoreStatus) proto.CodebaseIndexStatus {
	result := proto.CodebaseIndexStatus{
		Enabled:          settings.IsEnabled(),
		State:            string(status.State),
		Serving:          status.Serving,
		ProjectRoot:      status.ProjectRoot,
		DatabasePath:     status.DatabasePath,
		StoreDirectory:   status.StoreDirectory,
		SourceMode:       status.SourceMode,
		CredentialStatus: status.CredentialStatus,
		Model:            status.Model,
		IncludePaths:     append([]string(nil), settings.IncludePaths...),
		ExcludePaths:     append([]string(nil), settings.ExcludePaths...),
		FilesTotal:       status.FilesTotal,
		FilesProcessed:   status.FilesProcessed,
		ChunksCreated:    status.ChunksCreated,
		FilesSkipped:     status.FilesSkipped,
		CurrentPath:      status.CurrentPath,
		Stage:            status.Stage,
		StartedAt:        status.StartedAt,
		FinishedAt:       status.FinishedAt,
	}
	if status.Err != nil {
		result.Error = status.Err.Error()
	}
	return result
}

// -- Project lifecycle --

func (w *AppWorkspace) ProjectNeedsInitialization() (bool, error) {
	return config.ProjectNeedsInitialization(w.store)
}

func (w *AppWorkspace) MarkProjectInitialized() error {
	return config.MarkProjectInitialized(w.store)
}

func (w *AppWorkspace) InitializePrompt() (string, error) {
	return agent.InitializePrompt(w.store)
}

func (w *AppWorkspace) ListProjects(_ context.Context) ([]proto.ProjectInfo, error) {
	service := projects.NewService()
	documents, err := service.List()
	if err != nil {
		return nil, err
	}
	active, hasActive, err := service.Active(w.store.WorkingDir())
	if err != nil {
		return nil, err
	}
	result := make([]proto.ProjectInfo, len(documents))
	for index, document := range documents {
		completed := 0
		for _, task := range document.Tasks {
			if task.Completed {
				completed++
			}
		}
		result[index] = proto.ProjectInfo{
			Slug:      document.Metadata.Slug,
			Name:      document.Metadata.Name,
			Status:    string(document.Metadata.Status),
			Selected:  hasActive && active.Metadata.Slug == document.Metadata.Slug,
			Completed: completed,
			Total:     len(document.Tasks),
		}
	}
	return result, nil
}

func (w *AppWorkspace) SelectProject(_ context.Context, slug string) error {
	service := projects.NewService()
	if slug == "" {
		return service.Disable(w.store.WorkingDir())
	}
	_, err := service.Activate(slug, w.store.WorkingDir())
	return err
}

func (w *AppWorkspace) ListSkills(_ context.Context) ([]skills.CatalogEntry, error) {
	mgr := w.app.Skills
	return skills.Catalog(mgr.ActiveSkills(), mgr.ResolvedPaths(), mgr.WorkingDir()), nil
}

func (w *AppWorkspace) ReadSkill(_ context.Context, skillID string) ([]byte, skills.SkillReadResult, error) {
	mgr := w.app.Skills
	return skills.ReadContent(mgr.ActiveSkills(), mgr.ResolvedPaths(), mgr.WorkingDir(), skillID)
}

// -- MCP operations --

func (w *AppWorkspace) MCPGetStates() map[string]mcptools.ClientInfo {
	return mcptools.GetStates()
}

func (w *AppWorkspace) MCPRefreshPrompts(ctx context.Context, name string) {
	mcptools.RefreshPrompts(ctx, name)
}

func (w *AppWorkspace) MCPRefreshResources(ctx context.Context, name string) {
	mcptools.RefreshResources(ctx, name)
}

func (w *AppWorkspace) RefreshMCPTools(ctx context.Context, name string) {
	mcptools.RefreshTools(ctx, w.store, name)
}

func (w *AppWorkspace) ReadMCPResource(ctx context.Context, name, uri string) ([]MCPResourceContents, error) {
	contents, err := mcptools.ReadResource(ctx, w.store, name, uri)
	if err != nil {
		return nil, err
	}
	result := make([]MCPResourceContents, len(contents))
	for i, c := range contents {
		result[i] = MCPResourceContents{
			URI:      c.URI,
			MIMEType: c.MIMEType,
			Text:     c.Text,
			Blob:     c.Blob,
		}
	}
	return result, nil
}

func (w *AppWorkspace) ListMCPPrompts(context.Context) ([]commands.MCPPrompt, error) {
	return commands.LoadMCPPrompts()
}

func (w *AppWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	return commands.GetMCPPrompt(w.store, clientID, promptID, args)
}

func (w *AppWorkspace) EnableDockerMCP(ctx context.Context) error {
	mcpConfig, err := w.store.PrepareDockerMCPConfig()
	if err != nil {
		return err
	}

	if err := mcptools.InitializeSingle(ctx, config.DockerMCPName, w.store); err != nil {
		disableErr := mcptools.DisableSingle(w.store, config.DockerMCPName)
		w.store.RemoveDockerMCPInMemory()
		return fmt.Errorf("failed to start docker MCP: %w", errors.Join(err, disableErr))
	}

	if err := w.store.PersistDockerMCPConfig(mcpConfig); err != nil {
		disableErr := mcptools.DisableSingle(w.store, config.DockerMCPName)
		w.store.RemoveDockerMCPInMemory()
		return fmt.Errorf("docker MCP started but failed to persist configuration: %w", errors.Join(err, disableErr))
	}

	return nil
}

func (w *AppWorkspace) DisableDockerMCP() error {
	if err := mcptools.DisableSingle(w.store, config.DockerMCPName); err != nil {
		return fmt.Errorf("failed to disable docker MCP: %w", err)
	}
	return w.store.DisableDockerMCP()
}

func (w *AppWorkspace) MCPAuthenticate(ctx context.Context, name string) error {
	return mcptools.AuthenticateMCP(ctx, w.store, name)
}

func (w *AppWorkspace) MCPPendingAuth() []mcptools.PendingAuthServer {
	return mcptools.PendingAuthMCPs(w.store)
}

func (w *AppWorkspace) MCPAuthURL(name string) string {
	return mcptools.MCPAuthURL(name)
}

// -- Lifecycle --

func (w *AppWorkspace) Subscribe(program *tea.Program) {
	w.app.Subscribe(program)
}

func (w *AppWorkspace) Shutdown() {
	w.app.Shutdown()
}

// App returns the underlying app.App instance.
func (w *AppWorkspace) App() *app.App {
	return w.app
}

// Store returns the underlying config store.
func (w *AppWorkspace) Store() *config.ConfigStore {
	return w.store
}

// Compile-time check that AppWorkspace implements Workspace.
var _ Workspace = (*AppWorkspace)(nil)
