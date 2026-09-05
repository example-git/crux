// Package app wires together services, coordinates agents, and manages
// application lifecycle.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/agent/notify"
	"github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/automemory"
	"github.com/example-git/crux/internal/clipboard"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/filetracker"
	"github.com/example-git/crux/internal/format"
	"github.com/example-git/crux/internal/herdr"
	"github.com/example-git/crux/internal/history"
	"github.com/example-git/crux/internal/imagegen"
	"github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/lsp"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/shell"
	"github.com/example-git/crux/internal/skills"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/anim"
	"github.com/example-git/crux/internal/ui/styles"
)

type App struct {
	Sessions         session.Service
	Messages         message.Service
	History          history.Service
	Permissions      permission.Service
	Questions        question.Service
	FileTracker      filetracker.Service
	BackgroundShells *shell.BackgroundShellManager
	BackgroundAgents *agent.BackgroundAgentManager
	BackgroundImages *imagegen.JobManager
	TaskStore        *managedtask.Store

	AgentCoordinator agent.Coordinator
	agentInitMu      sync.RWMutex
	newCoordinator   func(context.Context, agent.CoordinatorOptions) (agent.Coordinator, error)

	LSPManager *lsp.Manager

	Skills *skills.Manager

	config *config.ConfigStore

	serviceEventsWG *sync.WaitGroup
	eventsCtx       context.Context
	events          *pubsub.Broker[tea.Msg]
	tuiWG           *sync.WaitGroup

	// global context and cleanup functions
	globalCtx          context.Context
	cleanupFuncs       []func(context.Context) error
	agentNotifications *pubsub.Broker[notify.Notification]
	// runCompletions is the authoritative per-run completion signal,
	// emitted once per top-level agent turn after all message
	// updates have been flushed. Bridged into app.events so SSE
	// subscribers (notably `crux run` in client/server mode) can
	// drive their exit on a deterministic, payload-bearing event
	// instead of guessing from message finish parts.
	runCompletions *pubsub.Broker[notify.RunComplete]

	// herdrClient reports agent state to herdr when running inside
	// a herdr-managed pane. Nil when not in a herdr environment.
	herdrClient                *herdr.Client
	shutdownOnce               sync.Once
	taskNotificationsOnce      sync.Once
	taskNotificationDeliveryMu sync.Mutex
	taskNotificationDeliveries map[string]struct{}
}

// New initializes a new application instance. skillsMgr carries the
// per-workspace skill discovery results computed by the caller; the
// caller is responsible for constructing it (typically via
// skills.NewManager + skills.DiscoverFromConfig).
func New(ctx context.Context, conn *sql.DB, store *config.ConfigStore, skillsMgr *skills.Manager) (*App, error) {
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q, message.WithCompactionStore(conn, sessions))
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot home directory: %w", err)
	}
	files, err := history.NewServiceWithSnapshots(q, conn, filepath.Join(homeDirectory, ".ai-cli", "file-snapshots"), store.WorkingDir())
	if err != nil {
		return nil, fmt.Errorf("initialize file snapshots: %w", err)
	}
	cfg := store.Config()
	skipPermissionsRequests := store.Overrides().SkipPermissionRequests
	var allowedTools []string
	if cfg.Permissions != nil && cfg.Permissions.AllowedTools != nil {
		allowedTools = cfg.Permissions.AllowedTools
	}
	var trustedPaths []string
	memory, memoryErr := automemory.Load(ctx, store.WorkingDir())
	if memoryErr != nil {
		var configurationError *automemory.ConfigurationError
		if errors.As(memoryErr, &configurationError) {
			return nil, fmt.Errorf("initialize auto memory: %w", memoryErr)
		}
		slog.Debug("Failed to initialize auto memory", "error", memoryErr)
	} else if memory.Managed && memory.Directory != "" {
		trustedPaths = append(trustedPaths, memory.Directory)
	}

	outputStore, err := managedtask.NewOutputStore(filepath.Join(cfg.Options.DataDirectory, "tasks", "output"), managedtask.OutputStoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("initialize task output storage: %w", err)
	}
	recordStore, err := managedtask.NewStore(filepath.Join(cfg.Options.DataDirectory, "tasks", "metadata"))
	if err != nil {
		_ = outputStore.Close()
		return nil, fmt.Errorf("initialize task metadata storage: %w", err)
	}
	backgroundShells, err := shell.NewBackgroundShellManagerWithStores(store.WorkingDir(), outputStore, recordStore)
	if err != nil {
		_ = recordStore.Close()
		_ = outputStore.Close()
		return nil, fmt.Errorf("recover background shells: %w", err)
	}
	backgroundAgents, err := agent.NewBackgroundAgentManagerWithStore(store.WorkingDir(), backgroundShells, recordStore)
	if err != nil {
		_ = recordStore.Close()
		_ = outputStore.Close()
		return nil, fmt.Errorf("initialize background agents: %w", err)
	}
	questions := question.NewService()
	imageRuntime, err := imagegen.NewHostPluginRuntime(ctx, store, imagegen.PluginCredentialBindings{})
	if err != nil {
		_ = recordStore.Close()
		_ = outputStore.Close()
		return nil, fmt.Errorf("initialize image plugins: %w", err)
	}
	backgroundImages, err := imagegen.NewJobManagerWithStore(store.WorkingDir(), recordStore, imagegen.JobManagerOptions{
		PluginRuntime: imageRuntime,
		Setup:         &imagegen.SetupService{Runtime: imageRuntime, Store: store, Questions: questions},
	})
	if err != nil {
		imageRuntime.Manager.Close()
		_ = recordStore.Close()
		_ = outputStore.Close()
		return nil, fmt.Errorf("recover background image jobs: %w", err)
	}

	app := &App{
		Sessions:         sessions,
		Messages:         messages,
		History:          files,
		Permissions:      permission.NewPermissionService(store.WorkingDir(), skipPermissionsRequests, allowedTools, trustedPaths...),
		Questions:        questions,
		FileTracker:      filetracker.NewService(q),
		BackgroundShells: backgroundShells,
		BackgroundAgents: backgroundAgents,
		BackgroundImages: backgroundImages,
		TaskStore:        recordStore,
		LSPManager:       lsp.NewManager(store),
		Skills:           skillsMgr,

		globalCtx: ctx,

		config: store,

		events:                     pubsub.NewBroker[tea.Msg](),
		serviceEventsWG:            &sync.WaitGroup{},
		tuiWG:                      &sync.WaitGroup{},
		agentNotifications:         pubsub.NewBroker[notify.Notification](),
		runCompletions:             pubsub.NewBroker[notify.RunComplete](),
		taskNotificationDeliveries: make(map[string]struct{}),
	}

	app.setupEvents()

	// Initialize clipboard support. This is best-effort; if it fails
	// (e.g., headless environment), clipboard operations will return nil.
	if err := clipboard.Init(); err != nil {
		slog.Warn("Clipboard initialization failed", "error", err)
	}

	// Arm initialization synchronously before launching it so WaitForInit
	// blocks for the in-flight init instead of racing the goroutine and
	// returning before any MCP tools register.
	mcp.ArmInit()
	go mcp.Initialize(ctx, app.Permissions, store)

	// Start herdr integration when running inside a herdr pane.
	app.herdrClient = herdr.Init()
	herdr.BridgeLocal(ctx, app.herdrClient, herdr.BridgeSources{
		PermRequests:      app.Permissions,
		PermNotifications: app.Permissions,
		RunCompletions:    app.runCompletions,
		Messages:          app.Messages,
	})

	// Release the shared database connection on shutdown. The pool
	// closes the underlying *sql.DB when the last reference is released.
	dataDir := cfg.Options.DataDirectory
	app.cleanupFuncs = append(
		app.cleanupFuncs,
		func(context.Context) error {
			imageRuntime.Manager.Close()
			return nil
		},
		func(context.Context) error { return db.Release(dataDir) },
		func(ctx context.Context) error { return mcp.Close(ctx) },
	)

	// TODO: remove the concept of agent config, most likely.
	if !cfg.CanInitializeAgent() {
		slog.Warn("Selected models are unavailable; starting without an agent")
		return app, nil
	}
	if err := app.InitCoderAgent(ctx); err != nil {
		return nil, fmt.Errorf("failed to initialize coder agent: %w", err)
	}

	// Set up callback for LSP state updates.
	app.LSPManager.SetCallback(func(name string, client *lsp.Client) {
		if client == nil {
			updateLSPState(name, lsp.StateUnstarted, nil, nil, 0)
			return
		}
		client.SetDiagnosticsCallback(updateLSPDiagnostics)
		updateLSPState(name, client.GetServerState(), nil, client, 0)
	})

	// TrackConfigured must run after SetCallback so the callback is already
	// installed when configured-but-not-yet-started LSPs are announced.
	go app.LSPManager.TrackConfigured(ctx)

	return app, nil
}

// Config returns the pure-data configuration.
func (app *App) Config() *config.Config {
	return app.config.Config()
}

// Store returns the config store.
func (app *App) Store() *config.ConfigStore {
	return app.config
}

func (app *App) CurrentAgentCoordinator() agent.Coordinator {
	app.agentInitMu.RLock()
	defer app.agentInitMu.RUnlock()
	return app.AgentCoordinator
}

func (app *App) ResetAgentSession(sessionID string) {
	if resetter, ok := app.CurrentAgentCoordinator().(interface{ ResetSession(string) }); ok {
		resetter.ResetSession(sessionID)
	}
}

// Events returns a per-caller subscription channel for application events.
// Each caller receives its own channel; all callers receive every event.
func (app *App) Events(ctx context.Context) <-chan pubsub.Event[tea.Msg] {
	return app.events.Subscribe(ctx)
}

// SendEvent publishes a message to all event subscribers.
func (app *App) SendEvent(msg tea.Msg) {
	app.events.Publish(pubsub.UpdatedEvent, msg)
}

// AgentNotifications returns the broker for agent notification events.
func (app *App) AgentNotifications() *pubsub.Broker[notify.Notification] {
	return app.agentNotifications
}

// RunCompletions returns the broker for the authoritative per-run
// terminal RunComplete events. The dispatcher (backend.runAgent) uses
// it to emit a reliable terminal event when a run fails before the
// coordinator could publish one of its own.
func (app *App) RunCompletions() *pubsub.Broker[notify.RunComplete] {
	return app.runCompletions
}

// ReportCurrentSession tells herdr which session the user is now
// viewing so it can persist a resumable reference for the pane. Safe
// to call when not running inside a herdr pane; the underlying client
// is nil-safe. Call this whenever the active session changes (load,
// new, or select).
func (app *App) ReportCurrentSession(sessionID string) {
	app.herdrClient.SetSessionID(sessionID)
}

// resolveSession resolves which session to use for a non-interactive run
// If continueSessionID is set, it looks up that session by ID
// If useLast is set, it returns the most recently updated top-level session
// Otherwise, it creates a new session
func (app *App) resolveSession(ctx context.Context, continueSessionID string, useLast bool) (session.Session, error) {
	switch {
	case continueSessionID != "":
		if app.Sessions.IsAgentToolSession(continueSessionID) {
			return session.Session{}, fmt.Errorf("cannot continue an agent tool session: %s", continueSessionID)
		}
		sess, err := app.Sessions.Get(ctx, continueSessionID)
		if err != nil {
			return session.Session{}, fmt.Errorf("session not found: %s", continueSessionID)
		}
		if sess.ParentSessionID != "" {
			return session.Session{}, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
		}
		return sess, nil

	case useLast:
		sess, err := app.Sessions.GetLast(ctx)
		if err != nil {
			return session.Session{}, fmt.Errorf("no sessions found to continue")
		}
		return sess, nil

	default:
		return app.Sessions.Create(ctx, agent.DefaultSessionName)
	}
}

func nonInteractivePermissionContext(ctx context.Context, autoApprove bool) context.Context {
	if autoApprove {
		return permission.WithRunApproval(ctx)
	}
	return permission.WithDetachedAgent(ctx)
}

// RunNonInteractive runs the application in non-interactive mode with the
// given prompt, printing to stdout.
func (app *App) RunNonInteractive(ctx context.Context, output io.Writer, prompt, largeModel, smallModel string, hideSpinner bool, continueSessionID string, useLast, autoApprove bool) error {
	slog.Info("Running in non-interactive mode")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if largeModel != "" || smallModel != "" {
		if err := app.overrideModelsForNonInteractive(largeModel, smallModel); err != nil {
			return fmt.Errorf("failed to override models: %w", err)
		}
	}

	// Re-initialize the coder agent without interactive-only tools.
	coordinator, err := app.initCoderAgent(ctx, false)
	if err != nil {
		return fmt.Errorf("failed to reinitialize agent for non-interactive mode: %w", err)
	}

	var (
		spinner   *format.Spinner
		stderrTTY bool
		progress  bool
	)

	stderrTTY = term.IsTerminal(os.Stderr.Fd())
	progress = app.config.Config().Options.Progress == nil || *app.config.Config().Options.Progress

	if !hideSpinner && stderrTTY {
		t := styles.ThemeForProvider(app.config.Config().Models[config.SelectedModelTypeLarge].Provider)

		spinner = format.NewSpinner(ctx, cancel, anim.Settings{
			Size:        10,
			Label:       "Generating",
			GradColorA:  t.WorkingGradFromColor,
			GradColorB:  t.WorkingGradToColor,
			CycleColors: true,
		})
		spinner.Start()
	}

	// Helper function to stop spinner once.
	stopSpinner := func() {
		if !hideSpinner && spinner != nil {
			spinner.Stop()
			spinner = nil
		}
	}

	// Non-interactive runs get a single shot at the tool palette, so wait for
	// MCP initialization to settle before reading MCP tools. The coordinator
	// waits again for the same reason (it is the gate the client/server path
	// goes through); doing it here too surfaces the failure before we create a
	// session, and lets the UpdateModels below see every MCP tool.
	if err := mcp.WaitForInit(ctx); err != nil {
		return fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	// force update of agent models before running so mcp tools are loaded
	coordinator.UpdateModels(ctx)

	defer stopSpinner()

	sess, err := app.resolveSession(ctx, continueSessionID, useLast)
	if err != nil {
		return fmt.Errorf("failed to create session for non-interactive mode: %w", err)
	}

	if continueSessionID != "" || useLast {
		slog.Info("Continuing session for non-interactive run", "session_id", sess.ID)
		// If no explicit model override was requested, restore the
		// model/provider from the last assistant message in the
		// session, provided it is still available.
		if largeModel == "" && smallModel == "" {
			if err := app.restoreModelFromSession(ctx, sess.ID); err != nil {
				slog.Warn("Failed to restore model from session", "session_id", sess.ID)
			}
		}
	} else {
		slog.Info("Created session for non-interactive run", "session_id", sess.ID)
	}

	// Report session identity to herdr.
	app.ReportCurrentSession(sess.ID)

	type response struct {
		result *fantasy.AgentResult
		err    error
	}
	done := make(chan response, 1)

	runCtx := nonInteractivePermissionContext(ctx, autoApprove)
	go func(ctx context.Context, sessionID, prompt string) {
		result, err := coordinator.Run(ctx, sess.ID, prompt)
		if err != nil {
			done <- response{
				err: fmt.Errorf("failed to start agent processing stream: %w", err),
			}
			return
		}
		done <- response{
			result: result,
		}
	}(runCtx, sess.ID, prompt)

	messageEvents := app.Messages.Subscribe(ctx)
	messageReadBytes := make(map[string]int)
	var printed bool

	defer func() {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(os.Stderr, ansi.ResetProgressBar)
		}

		// Always print a newline at the end. If output is a TTY this will
		// prevent the prompt from overwriting the last line of output.
		_, _ = fmt.Fprintln(output)
	}()

	for {
		if progress && stderrTTY {
			// HACK: Reinitialize the terminal progress bar on every iteration
			// so it doesn't get hidden by the terminal due to inactivity.
			_, _ = fmt.Fprintf(os.Stderr, ansi.SetIndeterminateProgressBar)
		}

		select {
		case result := <-done:
			stopSpinner()
			if result.err != nil {
				if errors.Is(result.err, context.Canceled) || errors.Is(result.err, agent.ErrRequestCancelled) {
					slog.Debug("Non-interactive: agent processing cancelled", "session_id", sess.ID)
					return nil
				}
				return fmt.Errorf("agent processing failed: %w", result.err)
			}
			return nil

		case event := <-messageEvents:
			msg := event.Payload
			if msg.SessionID == sess.ID && msg.Role == message.Assistant && len(msg.Parts) > 0 {
				stopSpinner()

				content := msg.Content().String()
				readBytes := messageReadBytes[msg.ID]

				if len(content) < readBytes {
					slog.Error("Non-interactive: message content is shorter than read bytes", "message_length", len(content), "read_bytes", readBytes)
					return fmt.Errorf("message content is shorter than read bytes: %d < %d", len(content), readBytes)
				}

				part := content[readBytes:]
				// Trim leading whitespace. Sometimes the LLM includes leading
				// formatting and intentation, which we don't want here.
				if readBytes == 0 {
					part = strings.TrimLeft(part, " \t")
				}
				// Ignore initial whitespace-only messages.
				if printed || strings.TrimSpace(part) != "" {
					printed = true
					fmt.Fprint(output, part)
				}
				messageReadBytes[msg.ID] = len(content)
			}

		case <-ctx.Done():
			stopSpinner()
			return ctx.Err()
		}
	}
}

// RewindSession rewinds a session to the given user message: every
// message from that point onward (inclusive) is deleted. When
// summarize is true, the conversation is summarized first and the
// summary message is preserved so the agent keeps condensed context. File
// snapshots are restored only when restoreFiles is true.
func (app *App) RewindSession(ctx context.Context, sessionID, messageID string, summarize, restoreFiles bool) error {
	coordinator := app.CurrentAgentCoordinator()
	if coordinator != nil && coordinator.IsSessionBusy(sessionID) {
		return errors.New("session is busy")
	}

	if summarize {
		if coordinator == nil {
			return errors.New("agent coordinator not initialized")
		}
		if err := coordinator.Summarize(ctx, sessionID); err != nil {
			return fmt.Errorf("failed to summarize before rewind: %w", err)
		}
	}

	if err := app.Messages.FlushAll(ctx); err != nil {
		return err
	}
	msgs, err := app.Messages.List(ctx, sessionID)
	if err != nil {
		return err
	}

	idx := -1
	for i, m := range msgs {
		if m.ID == messageID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("message %q not found in session %q", messageID, sessionID)
	}

	sess, err := app.Sessions.Get(ctx, sessionID)
	if err != nil {
		return err
	}

	checkpointTurnIDs := make([]string, 0, len(msgs)-idx)
	for _, msg := range msgs[idx:] {
		if msg.Role == message.User {
			checkpointTurnIDs = append(checkpointTurnIDs, msg.ID)
		}
	}
	if err := app.History.RewindCheckpoints(ctx, sessionID, checkpointTurnIDs, restoreFiles); err != nil {
		return fmt.Errorf("rewind file checkpoints: %w", err)
	}

	summaryDeleted := false
	for _, m := range msgs[idx:] {
		if summarize && m.ID == sess.SummaryMessageID {
			continue
		}
		if err := app.Messages.Delete(ctx, m.ID); err != nil {
			return err
		}
		if m.ID == sess.SummaryMessageID {
			summaryDeleted = true
		}
	}

	if summaryDeleted {
		sess.SummaryMessageID = ""
		if _, err := app.Sessions.Save(ctx, sess); err != nil {
			return err
		}
	}
	return nil
}

func (app *App) UpdateAgentModel(ctx context.Context, expected config.AgentModelState) error {
	coordinator := app.CurrentAgentCoordinator()
	if coordinator == nil {
		if err := app.config.RuntimeSnapshot().ValidateAgentModelState(expected); err != nil {
			return err
		}
		var err error
		coordinator, err = app.ensureCoderAgent(ctx, true)
		if err != nil {
			return fmt.Errorf("initialize coder agent: %w", err)
		}
	}
	return coordinator.UpdateModelsForState(ctx, expected)
}

// restoreModelFromSession reads the last assistant message in the
// session and, if it used a different provider/model than the current
// config, overrides the preferred model in-memory (non-persistent)
// provided the provider/model is still available. This ensures that
// continuing a session uses the same model that produced the last
// response.
func (app *App) restoreModelFromSession(ctx context.Context, sessionID string) error {
	lastMsg, err := app.Messages.GetLastAssistantMessage(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("failed to get last assistant message: %w", err)
	}
	if lastMsg.Provider == "" || lastMsg.Model == "" {
		return nil
	}

	cfg := app.config.Config()
	currentLarge := cfg.Models[config.SelectedModelTypeLarge]
	if currentLarge.Provider == lastMsg.Provider && currentLarge.Model == lastMsg.Model {
		return nil
	}

	if !cfg.IsModelAvailable(lastMsg.Provider, lastMsg.Model) {
		slog.Debug("Skipping model restoration: provider/model not available",
			"provider", lastMsg.Provider,
			"model", lastMsg.Model)
		return nil
	}

	if err := app.config.OverridePreferredModel(config.SelectedModelTypeLarge, config.SelectedModel{
		Provider: lastMsg.Provider,
		Model:    lastMsg.Model,
	}); err != nil {
		return fmt.Errorf("restore large model: %w", err)
	}
	if _, ok := cfg.Models[config.SelectedModelTypeSmall]; !ok {
		smallModel, err := app.GetDefaultSmallModel(lastMsg.Provider)
		if err != nil {
			return fmt.Errorf("resolve default small model: %w", err)
		}
		if err := app.config.OverridePreferredModel(config.SelectedModelTypeSmall, smallModel); err != nil {
			return fmt.Errorf("restore small model: %w", err)
		}
	}
	coordinator := app.CurrentAgentCoordinator()
	if coordinator == nil {
		return errors.New("agent coordinator not initialized")
	}
	if err := coordinator.UpdateModels(ctx); err != nil {
		return fmt.Errorf("failed to update agent models: %w", err)
	}
	slog.Info("Restored model from session",
		"provider", lastMsg.Provider,
		"model", lastMsg.Model)
	return nil
}

// overrideModelsForNonInteractive parses the model strings and temporarily
// overrides the model configurations before the agent is built.
// Format: "model-name" (searches all providers) or "provider/model-name".
// Model matching is case-insensitive.
// If largeModel is provided but smallModel is not, the small model defaults to
// the provider's default small model.
func (app *App) overrideModelsForNonInteractive(largeModel, smallModel string) error {
	cfg := app.config.Config()
	largeMatches, smallMatches, err := findModels(cfg, largeModel, smallModel)
	if err != nil {
		return err
	}

	var largeMatch, smallMatch modelMatch
	if largeModel != "" {
		largeMatch, err = validateMatches(largeMatches, largeModel, "large")
		if err != nil {
			return err
		}
	}
	if smallModel != "" {
		smallMatch, err = validateMatches(smallMatches, smallModel, "small")
		if err != nil {
			return err
		}
	}

	if largeModel != "" {
		slog.Info("Overriding large model for non-interactive run", "provider", largeMatch.provider, "model", largeMatch.modelID)
		if err := app.config.OverridePreferredModel(config.SelectedModelTypeLarge, config.SelectedModel{
			Provider: largeMatch.provider,
			Model:    largeMatch.modelID,
		}); err != nil {
			return fmt.Errorf("override large model: %w", err)
		}
	}

	switch {
	case smallModel != "":
		slog.Info("Overriding small model for non-interactive run", "provider", smallMatch.provider, "model", smallMatch.modelID)
		if err := app.config.OverridePreferredModel(config.SelectedModelTypeSmall, config.SelectedModel{
			Provider: smallMatch.provider,
			Model:    smallMatch.modelID,
		}); err != nil {
			return fmt.Errorf("override small model: %w", err)
		}

	case largeModel != "":
		smallCfg, err := app.GetDefaultSmallModel(largeMatch.provider)
		if err != nil {
			return err
		}
		if err := app.config.OverridePreferredModel(config.SelectedModelTypeSmall, smallCfg); err != nil {
			return fmt.Errorf("override small model: %w", err)
		}
	}

	return nil
}

// GetDefaultSmallModel returns the default small model for the given
// provider. Falls back to the large model if no default is found.
func (app *App) GetDefaultSmallModel(providerID string) (config.SelectedModel, error) {
	cfg := app.config.Config()
	knownProviders, _ := config.Providers(cfg)
	return defaultSmallModel(cfg, providerID, knownProviders)
}

func defaultSmallModel(cfg *config.Config, providerID string, knownProviders []catalog.Provider) (config.SelectedModel, error) {
	provider, ok := cfg.Providers.Get(providerID)
	if !ok || !cfg.IsProviderAvailable(providerID) {
		return config.SelectedModel{}, fmt.Errorf("provider %s is not available", providerID)
	}
	if len(provider.Models) == 0 {
		return config.SelectedModel{}, fmt.Errorf("provider %s has no models configured", providerID)
	}

	for _, known := range knownProviders {
		if string(known.ID) != providerID {
			continue
		}
		if model := cfg.GetModel(providerID, known.DefaultSmallModelID); model != nil {
			return config.SelectedModel{
				Provider:        providerID,
				Model:           model.ID,
				MaxTokens:       model.DefaultMaxTokens,
				ReasoningEffort: model.DefaultReasoningEffort,
			}, nil
		}
		break
	}

	large := cfg.Models[config.SelectedModelTypeLarge]
	if large.Provider == providerID {
		if model := cfg.GetModel(providerID, large.Model); model != nil {
			return large, nil
		}
	}

	model := provider.Models[0]
	return config.SelectedModel{
		Provider:        providerID,
		Model:           model.ID,
		MaxTokens:       model.DefaultMaxTokens,
		ReasoningEffort: model.DefaultReasoningEffort,
	}, nil
}

func (app *App) setupEvents() {
	ctx, cancel := context.WithCancel(app.globalCtx)
	app.eventsCtx = ctx
	setupSubscriber(ctx, app.serviceEventsWG, "sessions", app.Sessions.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "messages", app.Messages.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "permissions", app.Permissions.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "permissions-notifications", app.Permissions.SubscribeNotifications, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "question-batches", app.Questions.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "question-notifications", app.Questions.SubscribeNotifications, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "history", app.History.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "agent-notifications", app.agentNotifications.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "run-completions", app.runCompletions.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "shell-task-notifications", app.BackgroundShells.SubscribeNotifications, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "agent-task-notifications", app.BackgroundAgents.SubscribeNotifications, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "image-task-notifications", app.BackgroundImages.SubscribeNotifications, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "mcp", mcp.SubscribeEvents, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "lsp", SubscribeLSPEvents, app.events)
	if app.Skills != nil {
		setupSubscriber(ctx, app.serviceEventsWG, "skills", app.Skills.SubscribeEvents, app.events)
	}
	cleanupFunc := func(context.Context) error {
		cancel()
		app.serviceEventsWG.Wait()
		app.events.Shutdown()
		return nil
	}
	app.cleanupFuncs = append(app.cleanupFuncs, cleanupFunc)
}

func setupSubscriber[T any](
	ctx context.Context,
	wg *sync.WaitGroup,
	name string,
	subscriber func(context.Context) <-chan pubsub.Event[T],
	broker *pubsub.Broker[tea.Msg],
) {
	wg.Go(func() {
		subCh := subscriber(ctx)
		for {
			select {
			case event, ok := <-subCh:
				if !ok {
					slog.Debug("Subscription channel closed", "name", name)
					return
				}
				broker.Publish(pubsub.UpdatedEvent, tea.Msg(event))
			case <-ctx.Done():
				slog.Debug("Subscription cancelled", "name", name)
				return
			}
		}
	})
}

// setupSubscriberMustDeliver is the bounded-blocking fan-in variant of
// setupSubscriber: it re-publishes upstream events onto the shared
// app.events broker using PublishMustDeliver instead of Publish. Use
// this for terminal events that subscribers cannot tolerate losing —
// notably RunComplete, which is the authoritative end-of-run signal
// for `crux run`. A lossy fan-in here can drop the only terminal
// event and hang non-interactive clients waiting on it.
func setupSubscriberMustDeliver[T any](
	ctx context.Context,
	wg *sync.WaitGroup,
	name string,
	subscriber func(context.Context) <-chan pubsub.Event[T],
	broker *pubsub.Broker[tea.Msg],
) {
	wg.Go(func() {
		subCh := subscriber(ctx)
		for {
			select {
			case event, ok := <-subCh:
				if !ok {
					slog.Debug("Subscription channel closed", "name", name)
					return
				}
				broker.PublishMustDeliver(ctx, pubsub.UpdatedEvent, tea.Msg(event))
			case <-ctx.Done():
				slog.Debug("Subscription cancelled", "name", name)
				return
			}
		}
	})
}

func (app *App) InitCoderAgent(ctx context.Context) error {
	_, err := app.initCoderAgent(ctx, true)
	return err
}

// InitCoderAgentNonInteractive initializes the coder agent without
// interactive-only tools (e.g. question).
func (app *App) InitCoderAgentNonInteractive(ctx context.Context) error {
	_, err := app.initCoderAgent(ctx, false)
	return err
}

func (app *App) initCoderAgent(ctx context.Context, interactive bool) (agent.Coordinator, error) {
	app.agentInitMu.Lock()
	defer app.agentInitMu.Unlock()
	return app.newCoderAgentLocked(ctx, interactive)
}

func (app *App) ensureCoderAgent(ctx context.Context, interactive bool) (agent.Coordinator, error) {
	app.agentInitMu.Lock()
	defer app.agentInitMu.Unlock()
	if app.AgentCoordinator != nil {
		return app.AgentCoordinator, nil
	}
	return app.newCoderAgentLocked(ctx, interactive)
}

func (app *App) newCoderAgentLocked(ctx context.Context, interactive bool) (agent.Coordinator, error) {
	coderAgentCfg := app.config.Config().Agents[config.AgentCoder]
	if coderAgentCfg.ID == "" {
		return nil, fmt.Errorf("coder agent configuration is missing")
	}
	newCoordinator := app.newCoordinator
	if newCoordinator == nil {
		newCoordinator = agent.NewCoordinator
	}
	coordinator, err := newCoordinator(ctx, agent.CoordinatorOptions{
		Config:           app.config,
		Sessions:         app.Sessions,
		Messages:         app.Messages,
		Permissions:      app.Permissions,
		Questions:        app.Questions,
		History:          app.History,
		FileTracker:      app.FileTracker,
		LSPManager:       app.LSPManager,
		Notify:           app.agentNotifications,
		RunComplete:      app.runCompletions,
		Skills:           app.Skills,
		Interactive:      interactive,
		BackgroundShells: app.BackgroundShells,
		BackgroundAgents: app.BackgroundAgents,
		BackgroundImages: app.BackgroundImages,
	})
	if err != nil {
		slog.Error("Failed to create coder agent")
		return nil, err
	}
	app.AgentCoordinator = coordinator
	app.startTaskNotificationDelivery()
	return coordinator, nil
}

// Subscribe sends events to the TUI as tea.Msgs.
func (app *App) Subscribe(program *tea.Program) {
	defer log.RecoverPanic("app.Subscribe", func() {
		slog.Info("TUI subscription panic: attempting graceful shutdown")
		program.Quit()
	})

	app.tuiWG.Add(1)
	tuiCtx, tuiCancel := context.WithCancel(app.globalCtx)
	app.cleanupFuncs = append(app.cleanupFuncs, func(context.Context) error {
		slog.Debug("Cancelling TUI message handler")
		tuiCancel()
		app.tuiWG.Wait()
		return nil
	})
	defer app.tuiWG.Done()

	events := app.events.Subscribe(tuiCtx)
	for {
		select {
		case <-tuiCtx.Done():
			slog.Debug("TUI message handler shutting down")
			return
		case ev, ok := <-events:
			if !ok {
				slog.Debug("TUI message channel closed")
				return
			}
			program.Send(ev.Payload)
		}
	}
}

// Shutdown performs a graceful shutdown of the application.
func (app *App) Shutdown() {
	app.shutdownOnce.Do(app.shutdown)
}

func (app *App) shutdown() {
	start := time.Now()
	defer func() { slog.Debug("Shutdown took " + time.Since(start).String()) }()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First, cancel all agents and wait for them to finish. This must complete
	// before closing the DB so agents can finish writing their state.
	if coordinator := app.CurrentAgentCoordinator(); coordinator != nil {
		coordinator.CancelAll()
		if closer, ok := coordinator.(interface{ CloseContext(context.Context) }); ok {
			closer.CloseContext(shutdownCtx)
		} else if closer, ok := coordinator.(interface{ Close() }); ok {
			closer.Close()
		}
	}

	// Drain any debounced message updates before the DB-close cleanup
	// runs in the parallel block below. message.Service buffers
	// streaming deltas (see internal/message/message.go) and we must
	// land them while the connection is still open.
	if app.Messages != nil {
		if err := app.Messages.FlushAll(shutdownCtx); err != nil {
			slog.Error("Failed to flush pending message updates on shutdown", "error", err)
		}
	}

	// Now run remaining cleanup tasks in parallel.
	var wg sync.WaitGroup

	// Kill all background shells.
	wg.Go(func() {
		app.BackgroundShells.KillAll(shutdownCtx)
	})
	if app.BackgroundImages != nil {
		wg.Go(func() {
			app.BackgroundImages.StopAll(shutdownCtx)
		})
	}

	// Close herdr client to stop its background writer.
	app.herdrClient.Close()

	// Shutdown all LSP clients.
	wg.Go(func() {
		app.LSPManager.KillAll(shutdownCtx)
	})

	// Call all cleanup functions.
	for _, cleanup := range app.cleanupFuncs {
		if cleanup != nil {
			wg.Go(func() {
				if err := cleanup(shutdownCtx); err != nil {
					slog.Error("Failed to cleanup app properly on shutdown", "error", err)
				}
			})
		}
	}
	wg.Wait()
	if app.TaskStore != nil {
		if err := app.TaskStore.Close(); err != nil {
			slog.Error("Failed to close task metadata storage", "error", err)
		}
	}
}
