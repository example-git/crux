package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/anthropic"
	"github.com/example-git/crux/internal/agent/notify"
	"github.com/example-git/crux/internal/agent/prompt"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/automemory"
	"github.com/example-git/crux/internal/codebaseindex"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/copilotinference"
	"github.com/example-git/crux/internal/discover"
	"github.com/example-git/crux/internal/filetracker"
	"github.com/example-git/crux/internal/history"
	"github.com/example-git/crux/internal/hooks"
	"github.com/example-git/crux/internal/imageattachment"
	"github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/lsp"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/codex"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/projects"
	"github.com/example-git/crux/internal/providerregistry"
	anthropictransport "github.com/example-git/crux/internal/providertransport/anthropic"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/shell"
	"github.com/example-git/crux/internal/skills"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/foundation/providers/openaicompat"
	openaisdk "github.com/openai/openai-go/v3/option"
	"github.com/qjebbs/go-jsons"
)

// Coordinator errors.
var (
	errCoderAgentNotConfigured         = errors.New("coder agent not configured")
	errModelProviderNotConfigured      = errors.New("model provider not configured")
	errLargeModelNotSelected           = errors.New("large model not selected")
	errSmallModelNotSelected           = errors.New("small model not selected")
	errLargeModelProviderNotConfigured = errors.New("large model provider not configured")
	errSmallModelProviderNotConfigured = errors.New("small model provider not configured")
	errLargeModelNotFound              = errors.New("large model not found in provider config")
	errSmallModelNotFound              = errors.New("small model not found in provider config")
)

// Copilot models that use the Responses API instead of Chat Completions.
var copilotResponsesModels = map[string]bool{
	"gpt-5.2":       true,
	"gpt-5.2-codex": true,
	"gpt-5.3-codex": true,
	"gpt-5.4":       true,
	"gpt-5.4-mini":  true,
	"gpt-5.5":       true,
	"gpt-5-mini":    true,
	"gpt-5.6-luna":  true,
	"gpt-5.6-terra": true,
	"gpt-5.6-sol":   true,
}

type Coordinator interface {
	// INFO: (kujtim) this is not used yet we will use this when we have multiple agents
	// SetMainAgent(string)
	Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	// RunAccepted runs a call that was already accepted via
	// BeginAccepted on the fire-and-forget dispatch path. The handle is
	// the only carrier of accept-state across the backend.runAgent /
	// Coordinator / sessionAgent.Run layers: it reaches
	// sessionAgent.Run as SessionAgentCall.Accepted, where it is
	// consumed under dispatchMu once the accepted -> (cancel-on-entry |
	// queued | active) transition is chosen.
	RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	BeginAccepted(sessionID string) *AcceptedRun
	Cancel(sessionID string)
	CancelAll()
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []QueuedPrompt
	ClearQueue(sessionID string)
	Summarize(context.Context, string) error
	Model() Model
	UpdateModels(ctx context.Context) error
	GenerateTitle(ctx context.Context, sessionID, prompt string)
	SuggestPrompt(ctx context.Context, sessionID string) (string, error)
}

type coordinator struct {
	cfg                      *config.ConfigStore
	sessions                 session.Service
	messages                 message.Service
	permissions              permission.Service
	questions                question.Service
	history                  history.Service
	filetracker              filetracker.Service
	lspManager               *lsp.Manager
	notify                   pubsub.Publisher[notify.Notification]
	runComplete              pubsub.Publisher[notify.RunComplete]
	interactive              bool
	automaticCodebaseContext func(context.Context, string) (string, error)
	loadAgentDefinitions     func(string, *config.Config) ([]agentDefinition, error)
	backgroundShells         *shell.BackgroundShellManager
	backgroundAgents         *BackgroundAgentManager

	currentAgent         SessionAgent
	systemPromptTemplate *prompt.Prompt
	agents               map[string]SessionAgent

	// Skills discovery results (session-start snapshot).
	allSkills    []*skills.Skill // Pre-filter: all discovered after dedup.
	activeSkills []*skills.Skill // Post-filter: active skills only.
	skillTracker *skills.Tracker

	readyWg errgroup.Group

	reasoningMu       sync.RWMutex
	reasoningDisabled map[string]bool
	codexSessions     *codexresponses.SessionStore
	memoryWorker      *automemory.Worker

	codebaseIndexReconcileMu sync.Mutex
	codebaseIndexReconciling bool
	reconcileCodebaseIndexFn func(context.Context) (codebaseindex.StoreStatus, error)
}

// CoordinatorOptions holds the dependencies for NewCoordinator. Using a
// struct keeps the constructor self-documenting and avoids a long
// positional parameter list.
type CoordinatorOptions struct {
	Config           *config.ConfigStore
	Sessions         session.Service
	Messages         message.Service
	Permissions      permission.Service
	Questions        question.Service
	History          history.Service
	FileTracker      filetracker.Service
	LSPManager       *lsp.Manager
	Notify           pubsub.Publisher[notify.Notification]
	RunComplete      pubsub.Publisher[notify.RunComplete]
	Skills           *skills.Manager
	Interactive      bool
	BackgroundShells *shell.BackgroundShellManager
	BackgroundAgents *BackgroundAgentManager
}

func NewCoordinator(ctx context.Context, opts CoordinatorOptions) (Coordinator, error) {
	if opts.BackgroundShells == nil {
		opts.BackgroundShells = shell.NewBackgroundShellManager(opts.Config.WorkingDir())
	}
	if opts.BackgroundAgents == nil {
		opts.BackgroundAgents = NewBackgroundAgentManager(opts.Config.WorkingDir(), opts.BackgroundShells)
	}

	// Skills are pre-discovered by the caller (see app.New /
	// backend.CreateWorkspace) and passed in via the manager. If no
	// manager was provided (legacy callers), fall back to an in-line
	// discovery so the coordinator still works.
	var allSkills, activeSkills []*skills.Skill
	if opts.Skills != nil {
		allSkills = opts.Skills.AllSkills()
		activeSkills = opts.Skills.ActiveSkills()
	} else {
		allSkills, activeSkills = discoverSkills(opts.Config)
	}
	skillTracker := skills.NewTracker(activeSkills)

	c := &coordinator{
		cfg:                  opts.Config,
		sessions:             opts.Sessions,
		messages:             opts.Messages,
		permissions:          opts.Permissions,
		questions:            opts.Questions,
		history:              opts.History,
		filetracker:          opts.FileTracker,
		lspManager:           opts.LSPManager,
		notify:               opts.Notify,
		runComplete:          opts.RunComplete,
		agents:               make(map[string]SessionAgent),
		allSkills:            allSkills,
		activeSkills:         activeSkills,
		skillTracker:         skillTracker,
		interactive:          opts.Interactive,
		reasoningDisabled:    make(map[string]bool),
		codexSessions:        codexresponses.NewSessionStore(),
		loadAgentDefinitions: discoverAgentDefinitions,
		backgroundShells:     opts.BackgroundShells,
		backgroundAgents:     opts.BackgroundAgents,
	}
	c.automaticCodebaseContext = c.retrieveAutomaticCodebaseContext

	agentCfg, ok := opts.Config.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	// TODO: make this dynamic when we support multiple agents
	prompt, err := coderPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	agent, err := c.buildAgent(ctx, prompt, agentCfg, false)
	if err != nil {
		return nil, err
	}
	c.currentAgent = agent
	c.systemPromptTemplate = prompt
	c.agents[config.AgentCoder] = agent
	memory, err := automemory.Load(ctx, c.cfg.WorkingDir())
	if err != nil {
		return nil, err
	}
	c.memoryWorker, err = automemory.NewWorker(automemory.WorkerOptions{
		Memory: memory,
		Generate: func(workerCtx context.Context, purpose, memoryPrompt string, maxOutputTokens int64) (string, error) {
			return c.currentAgent.GenerateMemory(workerCtx, purpose, memoryPrompt, maxOutputTokens)
		},
		LoadTranscript: c.loadMemoryTranscript,
		LoadSessions:   c.loadMemorySessions,
	})
	if err != nil {
		return nil, err
	}
	c.scheduleCodebaseIndexReconcile(ctx)
	return c, nil
}

func (c *coordinator) loadMemoryTranscript(ctx context.Context, sessionID string) ([]automemory.Turn, error) {
	if err := c.messages.FlushAll(ctx); err != nil {
		return nil, err
	}
	currentSession, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var messages []message.Message
	if currentSession.SummaryMessageID != "" {
		messages, err = c.messages.ListFrom(ctx, sessionID, currentSession.SummaryMessageID)
	} else {
		messages, err = c.messages.List(ctx, sessionID)
	}
	if err != nil {
		return nil, err
	}
	turns := make([]automemory.Turn, 0, len(messages))
	for _, item := range messages {
		var role string
		switch item.Role {
		case message.User:
			role = "user"
		case message.Assistant:
			role = "assistant"
		default:
			continue
		}
		if text := strings.TrimSpace(item.Content().Text); text != "" {
			turns = append(turns, automemory.Turn{Role: role, Text: text})
		}
	}
	return turns, nil
}

func (c *coordinator) loadMemorySessions(ctx context.Context) ([]automemory.SessionInfo, error) {
	sessions, err := c.sessions.List(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]automemory.SessionInfo, 0, len(sessions))
	for _, item := range sessions {
		if item.ParentSessionID != "" {
			continue
		}
		result = append(result, automemory.SessionInfo{
			ID:        item.ID,
			UpdatedAt: time.Unix(item.UpdatedAt, 0),
		})
	}
	return result, nil
}

func (c *coordinator) scheduleCodebaseIndexReconcile(ctx context.Context) {
	c.codebaseIndexReconcileMu.Lock()
	if c.codebaseIndexReconciling {
		c.codebaseIndexReconcileMu.Unlock()
		return
	}
	c.codebaseIndexReconciling = true
	reconcile := c.reconcileCodebaseIndexFn
	if reconcile == nil {
		reconcile = c.reconcileCodebaseIndex
	}
	c.codebaseIndexReconcileMu.Unlock()

	go func() {
		defer func() {
			c.codebaseIndexReconcileMu.Lock()
			c.codebaseIndexReconciling = false
			c.codebaseIndexReconcileMu.Unlock()
		}()
		if _, err := reconcile(context.WithoutCancel(ctx)); err != nil {
			slog.Warn("Could not reconcile background codebase indexing", "error", err)
		}
	}()
}

type codebaseIndexController interface {
	codebaseIndexStatus(context.Context) (codebaseindex.StoreStatus, error)
	reconcileCodebaseIndex(context.Context) (codebaseindex.StoreStatus, error)
}

func CodebaseIndexStatus(ctx context.Context, coordinator Coordinator) (codebaseindex.StoreStatus, error) {
	controller, ok := coordinator.(codebaseIndexController)
	if !ok {
		return codebaseindex.StoreStatus{}, fmt.Errorf("codebase index controller is unavailable")
	}
	return controller.codebaseIndexStatus(ctx)
}

func ReconcileCodebaseIndex(ctx context.Context, coordinator Coordinator) (codebaseindex.StoreStatus, error) {
	controller, ok := coordinator.(codebaseIndexController)
	if !ok {
		return codebaseindex.StoreStatus{}, fmt.Errorf("codebase index controller is unavailable")
	}
	return controller.reconcileCodebaseIndex(ctx)
}

func AutoMemoryActivity(coordinator Coordinator) string {
	controller, ok := coordinator.(interface{ autoMemoryActivity() string })
	if !ok {
		return ""
	}
	return controller.autoMemoryActivity()
}

func (c *coordinator) autoMemoryActivity() string {
	return c.memoryWorker.Activity()
}

func (c *coordinator) codebaseIndexOptions(ctx context.Context) (codebaseindex.ProjectIndexOptions, error) {
	projectRoot, err := codebaseindex.CanonicalProjectRoot(ctx, c.cfg.WorkingDir())
	if err != nil {
		return codebaseindex.ProjectIndexOptions{}, err
	}
	toolConfig := c.cfg.Config().Tools.CodebaseSearch
	return codebaseindex.ProjectIndexOptions{
		ProjectRoot:            projectRoot,
		ConfiguredDatabasePath: toolConfig.DatabasePath,
		StoreDirectory:         toolConfig.GetStoreDirectory(),
		Enabled:                toolConfig.IsEnabled(),
		Filters: codebaseindex.ProjectFilters{
			IncludePaths: toolConfig.IncludePaths,
			ExcludePaths: toolConfig.ExcludePaths,
		},
	}, nil
}

func (c *coordinator) codebaseIndexStatus(ctx context.Context) (codebaseindex.StoreStatus, error) {
	options, err := c.codebaseIndexOptions(ctx)
	if err != nil {
		return codebaseindex.StoreStatus{}, err
	}
	return codebaseindex.InspectProjectIndexStatus(options), nil
}

func (c *coordinator) reconcileCodebaseIndex(ctx context.Context) (codebaseindex.StoreStatus, error) {
	options, err := c.codebaseIndexOptions(ctx)
	if err != nil {
		return codebaseindex.StoreStatus{}, err
	}
	status := codebaseindex.ReconcileProjectIndexing(ctx, options)
	if status.State == codebaseindex.StoreStateFailed {
		slog.Warn("Could not start background codebase indexing", "error", status.Err)
	}
	return status, nil
}

// Run implements Coordinator.
func (c *coordinator) Run(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, nil, sessionID, prompt, attachments...)
}

// RunAccepted implements Coordinator.
func (c *coordinator) RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, accept, sessionID, prompt, attachments...)
}

// run is the shared implementation behind Run and RunAccepted. When
// accept is non-nil it is threaded onto the SessionAgentCall as
// Accepted so sessionAgent.Run can consume the accept reservation under
// dispatchMu; when nil (the in-process/local path) no accept tracking
// applies.
func (c *coordinator) run(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}
	c.memoryWorker.Interrupt()

	codebaseContext := c.startAutomaticCodebaseContext(ctx, prompt)
	if codebaseContext != nil {
		defer codebaseContext.cancel()
	}

	// MCP servers connect asynchronously (see mcp.Initialize).
	//
	// Interactive runs never wait for that to finish: the tool list below
	// is built from whatever is registered right now, servers still
	// connecting are simply absent from this run's palette, and they are
	// picked up by later runs once they register and publish
	// EventToolsListChanged. Blocking here froze the TUI for the duration
	// of the slowest server's connect timeout whenever a prompt was sent
	// before initialization finished — most visibly on the first message.
	//
	// Non-interactive runs get a single shot at the tool palette, so they
	// do wait for initialization to settle. The wait is bounded by each
	// server's own connect timeout, so a hung server cannot stall the run
	// indefinitely.
	if !c.interactive {
		if err := mcp.WaitForInit(ctx); err != nil {
			return nil, fmt.Errorf("failed to wait for MCP initialization: %w", err)
		}
	}

	// refresh models before each run
	if err := c.UpdateModels(ctx); err != nil {
		return nil, fmt.Errorf("failed to update models: %w", err)
	}

	model := c.currentAgent.Model()
	normalizedAttachments, err := imageattachment.NormalizeAll(model.ModelCfg.Provider, attachments)
	if err != nil {
		return nil, err
	}
	attachments = normalizedAttachments
	if c.systemPromptTemplate != nil {
		systemPrompt, err := c.systemPromptTemplate.Build(ctx, model.ModelCfg.Provider, model.Model.Model(), c.cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to refresh system prompt: %w", err)
		}
		c.currentAgent.SetSystemPrompt(systemPrompt)
	}
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}

	mergedOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)
	mergedOptions = c.oauthReasoningOptions(providerCfg.ID, model.ModelCfg.Model, mergedOptions)
	mergedOptions = applyRegisteredRuntimeOptions(providerCfg.ID, model.ModelCfg.Model, c.cfg.Config().Options, mergedOptions)

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		// NOTE(@andreynering): We don't return here because the event handling to ask the user to reauthenticate
		// depends on the flow below. If refresh fails, proceed with the token we have.
		slog.Error("Failed to refresh OAuth2 token. Proceeding with existing token.", "provider", providerCfg.ID)
	}

	// Coalesce per-attempt RunComplete payloads so only the final
	// outcome reaches subscribers. Without this, the first attempt's
	// failed RunComplete (unauthorized) would race ahead of the
	// retry's success, and `crux run` would exit on the stale error
	// before ever seeing the retry result. Each attempt's
	// SessionAgentCall.OnComplete hook overwrites latest; we publish
	// exactly once after retries resolve, via PublishMustDeliver, so
	// a momentarily-full subscriber buffer can't silently drop the
	// terminal event.
	var (
		latest    notify.RunComplete
		hasLatest bool
	)
	onComplete := func(rc notify.RunComplete) {
		latest = rc
		hasLatest = true
	}
	// Propagate the caller-supplied RunID (set via agent.WithRunID
	// at the HTTP boundary in backend.SendMessage) onto the
	// SessionAgentCall so the terminal RunComplete event echoes it
	// back. Both attempts in the retry chain reuse the same RunID;
	// the coalesce closure publishes the final outcome under that
	// same correlator.
	runID := RunIDFromContext(ctx)
	submissionID := SubmissionIDFromContext(ctx)
	if submissionID == "" {
		submissionID = uuid.NewString()
	}
	turnInstructions := codebaseContext.wait()
	memoryContext, memoryErr := automemory.Relevant(ctx, c.cfg.WorkingDir(), prompt, time.Now())
	if memoryErr != nil {
		slog.Debug("Could not load relevant auto-memory", "error", memoryErr)
	} else if memoryContext != "" {
		if turnInstructions != "" {
			turnInstructions += "\n\n"
		}
		turnInstructions += memoryContext
	}
	run := func() (*fantasy.AgentResult, error) {
		return c.currentAgent.Run(ctx, SessionAgentCall{
			SessionID:        sessionID,
			SubmissionID:     submissionID,
			RunID:            runID,
			Prompt:           prompt,
			TurnInstructions: turnInstructions,
			Attachments:      attachments,
			MaxOutputTokens:  maxTokens,
			ProviderOptions:  mergedOptions,
			Temperature:      temp,
			TopP:             topP,
			TopK:             topK,
			FrequencyPenalty: freqPenalty,
			PresencePenalty:  presPenalty,
			OnComplete:       onComplete,
			OnProviderWarning: func(w fantasy.CallWarning) {
				c.disableOAuthReasoning(providerCfg.ID, model.ModelCfg.Model, w.Message)
			},
			Accepted:      accept,
			OnAuthRefresh: c.makeAuthRefreshCallback(providerCfg),
		})
	}
	beforeLoaded := c.skillTracker.LoadedNames()
	result, originalErr := run()
	if originalErr != nil {
		c.disableOAuthReasoning(providerCfg.ID, model.ModelCfg.Model, originalErr.Error())
	}
	logTurnSkillUsage(sessionID, prompt, c.activeSkills, c.skillTracker, beforeLoaded)
	if originalErr == nil {
		c.memoryWorker.Enqueue(sessionID)
	}

	if hasLatest && c.runComplete != nil {
		c.runComplete.PublishMustDeliver(ctx, pubsub.UpdatedEvent, latest)
		// Signal to the dispatcher (backend.runAgent) that the
		// authoritative terminal RunComplete for this run was already
		// emitted, so it does not publish a duplicate fallback for the
		// error it is about to receive.
		MarkRunCompletePublished(ctx)
	}
	return result, originalErr
}

// effectiveReasoningEffort returns the reasoning effort to apply for provider calls.
// It prefers the user-selected effort when valid, otherwise the model default when
// valid, and finally falls back to the first configured reasoning level.
func effectiveReasoningEffort(model Model) string {
	if !model.CatwalkCfg.CanReason {
		return ""
	}

	if effort := model.ModelCfg.ReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if effort := model.CatwalkCfg.DefaultReasoningEffort; effort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, effort) {
		return effort
	}
	if len(model.CatwalkCfg.ReasoningLevels) > 0 {
		return model.CatwalkCfg.ReasoningLevels[0]
	}
	return ""
}

func registeredReasoning(providerID string) (*providerregistry.ReasoningCapability, bool) {
	registration, ok := config.ProviderBehaviorCapabilities(providerID)
	if !ok || registration.ProviderID != providerID || registration.Reasoning == nil {
		return nil, false
	}
	return registration.Reasoning, true
}

func isUnsupportedReasoningMessage(message string) bool {
	message = strings.ToLower(message)
	if !strings.Contains(message, "reasoning") && !strings.Contains(message, "thinking") {
		return false
	}
	for _, marker := range []string{
		"unsupported",
		"not supported",
		"does not support",
		"invalid",
		"unknown",
		"unrecognized",
		"not allowed",
		"mutually exclusive",
		"error",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func oauthReasoningKey(providerID, modelID string) string {
	return providerID + "\x00" + modelID
}

func (c *coordinator) disableOAuthReasoning(providerID, modelID, message string) {
	reasoning, ok := registeredReasoning(providerID)
	if !ok || !reasoning.FallbackOnUnsupported || !isUnsupportedReasoningMessage(message) {
		return
	}

	key := oauthReasoningKey(providerID, modelID)
	c.reasoningMu.Lock()
	if c.reasoningDisabled == nil {
		c.reasoningDisabled = make(map[string]bool)
	}
	alreadyDisabled := c.reasoningDisabled[key]
	c.reasoningDisabled[key] = true
	c.reasoningMu.Unlock()
	if !alreadyDisabled {
		slog.Warn("Disabling reasoning for OAuth model after unsupported response", "provider", providerID, "model", modelID, "message", message)
	}
}

func (c *coordinator) oauthReasoningOptions(providerID, modelID string, options fantasy.ProviderOptions) fantasy.ProviderOptions {
	reasoning, ok := registeredReasoning(providerID)
	if !ok || reasoning.Disable == nil {
		return options
	}
	c.reasoningMu.RLock()
	disabled := c.reasoningDisabled[oauthReasoningKey(providerID, modelID)]
	c.reasoningMu.RUnlock()
	if !disabled {
		return options
	}

	return reasoning.Disable(options)
}

func applyRegisteredRuntimeOptions(providerID, modelID string, cfg *config.Options, options fantasy.ProviderOptions) fantasy.ProviderOptions {
	registration, ok := config.ProviderBehaviorCapabilities(providerID)
	if !ok || registration.ProviderID != providerID || registration.Runtime == nil ||
		registration.Runtime.Available == nil || !registration.Runtime.Available(modelID) ||
		registration.Runtime.Apply == nil || cfg == nil {
		return options
	}
	return registration.Runtime.Apply(providerregistry.RuntimeValues{
		ResponseVerbosity: cfg.ResponseVerbosity,
		AnalysisEffort:    cfg.AnalysisEffort,
	}, options)
}

func getProviderOptions(model Model, providerCfg config.ProviderConfig) fantasy.ProviderOptions {
	options := fantasy.ProviderOptions{}

	cfgOpts := []byte("{}")
	providerCfgOpts := []byte("{}")
	catwalkOpts := []byte("{}")

	if model.ModelCfg.ProviderOptions != nil {
		if data, err := json.Marshal(model.ModelCfg.ProviderOptions); err == nil {
			cfgOpts = data
		}
	}
	if providerCfg.ProviderOptions != nil {
		if data, err := json.Marshal(providerCfg.ProviderOptions); err == nil {
			providerCfgOpts = data
		}
	}
	if model.CatwalkCfg.Options.ProviderOptions != nil {
		if data, err := json.Marshal(model.CatwalkCfg.Options.ProviderOptions); err == nil {
			catwalkOpts = data
		}
	}

	got, err := jsons.Merge([]io.Reader{
		bytes.NewReader(catwalkOpts),
		bytes.NewReader(providerCfgOpts),
		bytes.NewReader(cfgOpts),
	})
	if err != nil {
		slog.Error("Could not merge call config", "err", err)
		return options
	}

	mergedOptions := make(map[string]any)
	if err := json.Unmarshal([]byte(got), &mergedOptions); err != nil {
		slog.Error("Could not create config for call", "err", err)
		return options
	}

	reasoningEffort := effectiveReasoningEffort(model)
	if reasoning, ok := registeredReasoning(providerCfg.ID); ok && reasoning.Options != nil {
		return reasoning.Options(model.CatwalkCfg.ID, reasoningEffort, model.CatwalkCfg.CanReason, mergedOptions)
	}

	if registration, ok := config.ProviderCapabilities().Lookup(providerCfg.ID); ok && registration.Construction == providerregistry.ConstructionAnthropicMessages {
		shouldSetEffort := model.CatwalkCfg.CanReason && reasoningEffort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, reasoningEffort)
		_, hasEffort := mergedOptions["effort"]
		_, hasThinking := mergedOptions["thinking"]
		switch {
		case !hasEffort && shouldSetEffort:
			mergedOptions["effort"] = reasoningEffort
		case !hasThinking && model.ModelCfg.Think:
			mergedOptions["thinking"] = map[string]any{"budget_tokens": 2000}
		}
		if _, hasDisplay := mergedOptions["thinking_display"]; !hasDisplay {
			_, hasEffort = mergedOptions["effort"]
			_, hasThinking = mergedOptions["thinking"]
			if hasEffort || hasThinking {
				mergedOptions["thinking_display"] = string(anthropic.ThinkingDisplaySummarized)
			}
		}
		if parsed, parseErr := anthropic.ParseOptions(mergedOptions); parseErr == nil {
			options[anthropic.Name] = parsed
		}
		return options
	}

	if registration, ok := config.ProviderCapabilities().Lookup(providerCfg.ID); ok && registration.Construction == providerregistry.ConstructionOpenAIResponses {
		if openai.IsResponsesReasoningModel(model.CatwalkCfg.ID) {
			if _, exists := mergedOptions["reasoning_effort"]; !exists && reasoningEffort != "" {
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
			mergedOptions["reasoning_summary"] = "auto"
			mergedOptions["include"] = []openai.IncludeType{openai.IncludeReasoningEncryptedContent}
		}
		if parsed, parseErr := openai.ParseResponsesOptions(mergedOptions); parseErr == nil {
			options[openai.Name] = parsed
		}
		return options
	}

	if providerCfg.Type != openaicompat.Name && !discover.IsKnownCustomProvider(string(providerCfg.Type)) {
		return options
	}

	extraBody := make(map[string]any)
	shouldSetEffort := model.CatwalkCfg.CanReason && reasoningEffort != "" && slices.Contains(model.CatwalkCfg.ReasoningLevels, reasoningEffort)
	if _, exists := mergedOptions["reasoning_effort"]; !exists && shouldSetEffort {
		switch providerCfg.ID {
		case string(catwalk.InferenceProviderIoNet):
			extraBody["reasoning"] = map[string]string{"effort": reasoningEffort}
		case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
			if !strings.HasPrefix(strings.ToLower(model.CatwalkCfg.ID), "minimax") {
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
		default:
			mergedOptions["reasoning_effort"] = reasoningEffort
		}
	}

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderIoNet):
		if _, exists := extraBody["reasoning"]; !exists && model.CatwalkCfg.CanReason {
			if model.ModelCfg.Think {
				extraBody["reasoning"] = map[string]string{"effort": "medium"}
			} else {
				extraBody["reasoning"] = map[string]string{"effort": "none"}
			}
		}
	case string(catwalk.InferenceProviderZAI), string(catwalk.InferenceProviderDeepSeek):
		if model.ModelCfg.Think || reasoningEffort != "" {
			extraBody["thinking"] = map[string]any{"type": "enabled"}
		} else {
			extraBody["thinking"] = map[string]any{"type": "disabled"}
		}
	case string(catwalk.InferenceProviderFireworks):
		if reasoningEffort == "" {
			if model.ModelCfg.Think {
				extraBody["thinking"] = map[string]any{"type": "enabled"}
			} else {
				extraBody["thinking"] = map[string]any{"type": "disabled"}
			}
		}
	case string(catwalk.InferenceProviderBaseten):
		extraBody["chat_template_args"] = map[string]any{"enable_thinking": model.ModelCfg.Think || reasoningEffort != "" && reasoningEffort != "none"}
	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		if strings.HasPrefix(strings.ToLower(model.CatwalkCfg.ID), "minimax") {
			if model.CatwalkCfg.CanReason && (model.ModelCfg.Think || reasoningEffort != "") {
				extraBody["thinking"] = map[string]any{"type": "adaptive"}
				extraBody["reasoning_split"] = true
			} else {
				extraBody["thinking"] = map[string]any{"type": "disabled"}
			}
		}
	case string(catwalk.InferenceProviderAlibabaSingapore), string(catwalk.InferenceProviderAlibabaUS):
		if model.CatwalkCfg.CanReason {
			extraBody["enable_thinking"] = model.ModelCfg.Think || reasoningEffort != ""
		}
	}

	mergedOptions["extra_body"] = extraBody
	if parsed, parseErr := openaicompat.ParseOptions(mergedOptions); parseErr == nil {
		options[openaicompat.Name] = parsed
	}
	return options
}

func mergeCallOptions(model Model, cfg config.ProviderConfig) (fantasy.ProviderOptions, *float64, *float64, *int64, *float64, *float64) {
	modelOptions := getProviderOptions(model, cfg)
	temp := cmp.Or(model.ModelCfg.Temperature, model.CatwalkCfg.Options.Temperature)
	topP := cmp.Or(model.ModelCfg.TopP, model.CatwalkCfg.Options.TopP)
	topK := cmp.Or(model.ModelCfg.TopK, model.CatwalkCfg.Options.TopK)
	freqPenalty := cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatwalkCfg.Options.FrequencyPenalty)
	presPenalty := cmp.Or(model.ModelCfg.PresencePenalty, model.CatwalkCfg.Options.PresencePenalty)
	return modelOptions, temp, topP, topK, freqPenalty, presPenalty
}

func (c *coordinator) systemPromptBuilder(promptTemplate *prompt.Prompt) SystemPromptBuilder {
	return func(ctx context.Context, current session.Session, model Model) (string, error) {
		lifecycle, err := promptLifecycle(current)
		if err != nil {
			return "", err
		}
		return promptTemplate.BuildLifecycle(ctx, model.ModelCfg.Provider, model.Model.Model(), c.cfg, lifecycle)
	}
}

func promptLifecycle(current session.Session) (prompt.Lifecycle, error) {
	lifecycle := prompt.Lifecycle{Plan: current.Plan}
	switch current.Mode {
	case "", session.ModeDefault:
		lifecycle.Stage = prompt.LifecycleDefault
	case session.ModePlan:
		lifecycle.Stage = prompt.LifecycleDraft
	case session.ModePlanRevision:
		lifecycle.Stage = prompt.LifecycleRevision
	case session.ModePlanExecution:
		lifecycle.Stage = prompt.LifecycleExecution
	default:
		return prompt.Lifecycle{}, fmt.Errorf("invalid session mode %q", current.Mode)
	}
	return lifecycle, nil
}

func (c *coordinator) buildAgent(ctx context.Context, promptTemplate *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	large, small, err := c.buildAgentModels(ctx, agent, isSubAgent)
	if err != nil {
		return nil, err
	}

	largeProviderCfg, _ := c.cfg.Config().Providers.Get(large.ModelCfg.Provider)
	result := NewSessionAgent(SessionAgentOptions{
		LargeModel:           large,
		SmallModel:           small,
		SystemPromptPrefix:   largeProviderCfg.SystemPromptPrefix,
		SystemPrompt:         "",
		SystemPromptBuilder:  c.systemPromptBuilder(promptTemplate),
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		IsYolo:               c.permissions.SkipRequests(),
		Sessions:             c.sessions,
		Messages:             c.messages,
		Tools:                nil,
		PlanModeTools:        nil,
		Notify:               c.notify,
		RunComplete:          c.runComplete,
	})

	// The readiness goroutines below perform one-time setup — building the
	// system prompt and the initial tool list — whose results the
	// coordinator needs for its whole lifetime, so they must survive the
	// caller's context being canceled. Several entry points build an agent
	// from a short-lived HTTP request context: the server's
	// InitAgent/UpdateAgent handlers, and UpdateModels -> buildTools ->
	// agentTool -> buildAgent for the sub-agent. The tool-list build reads
	// the MCP registry as it stands; servers still connecting are picked up
	// by later runs. WithoutCancel drops cancellation while keeping context
	// values; the work is local and always completes.
	initCtx := context.WithoutCancel(ctx)

	c.readyWg.Go(func() error {
		systemPrompt, err := promptTemplate.Build(initCtx, large.ModelCfg.Provider, large.Model.Model(), c.cfg)
		if err != nil {
			return err
		}
		result.SetSystemPrompt(systemPrompt)
		return nil
	})

	c.readyWg.Go(func() error {
		palettes, err := c.buildTools(initCtx, agent, isSubAgent)
		if err != nil {
			return err
		}
		result.SetTools(palettes.normal, palettes.planMode)
		return nil
	})

	return result, nil
}

type toolPalettes struct {
	normal   []fantasy.AgentTool
	planMode []fantasy.AgentTool
}

func (c *coordinator) buildTools(ctx context.Context, agent config.Agent, isSubAgent bool) (toolPalettes, error) {
	var allTools []fantasy.AgentTool
	if slices.Contains(agent.AllowedTools, AgentToolName) {
		agentTool, err := c.agentTool(ctx)
		if err != nil {
			return toolPalettes{}, err
		}
		allTools = append(allTools, agentTool)
	}

	if slices.Contains(agent.AllowedTools, tools.AgenticFetchToolName) {
		agenticFetchTool, err := c.agenticFetchTool(ctx, nil)
		if err != nil {
			return toolPalettes{}, err
		}
		allTools = append(allTools, agenticFetchTool)
	}

	logFile := filepath.Join(c.cfg.Config().Options.DataDirectory, "logs", "crux.log")
	memoryService := automemory.NewService(c.cfg.WorkingDir())
	projectService := projects.NewService()

	// Build hook runner if PreToolUse hooks are configured.
	var hookRunner *hooks.Runner
	if preToolHooks := c.cfg.Config().Hooks[hooks.EventPreToolUse]; len(preToolHooks) > 0 {
		hookRunner = hooks.NewRunner(preToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}

	allTools = append(
		allTools,
		tools.NewBashTool(c.backgroundShells, c.permissions, c.cfg.WorkingDir()),
		tools.NewCruxInfoTool(c.cfg, c.lspManager, c.allSkills, c.activeSkills, c.skillTracker),
		tools.NewCruxLogsTool(logFile),
		tools.NewTrafficLogsTool(),
		tools.NewGitInspectTool(c.cfg.WorkingDir()),
		tools.NewJobListTool(c, c.backgroundShells),
		tools.NewJobOutputTool(c, c.backgroundShells),
		tools.NewJobKillTool(c, c.backgroundShells),
		tools.NewTaskListTool(c),
		tools.NewTaskOutputTool(c),
		tools.NewTaskStopTool(c),
		tools.NewTaskContinueTool(c),
		tools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), nil),
		tools.NewEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewMultiEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), nil),
		tools.NewGlobTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Glob),
		tools.NewGrepTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Grep),
		tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Tools.Ls),
		tools.NewMemoryListTool(memoryService),
		tools.NewMemoryUpsertTool(memoryService),
		tools.NewMemoryRemoveTool(memoryService),
		tools.NewProjectCreateTool(projectService, c.cfg.WorkingDir()),
		tools.NewProjectStatusTool(projectService, c.cfg.WorkingDir()),
		tools.NewProjectUpdateTool(projectService, c.cfg.WorkingDir()),
		tools.NewProjectNotesTool(projectService, c.cfg.WorkingDir()),
		tools.NewProjectCompleteTool(projectService, c.cfg.WorkingDir()),
		tools.NewSkillListTool(c.activeSkills, c.cfg.Config().Options.SkillsPaths, c.cfg.WorkingDir(), c.skillTracker),
		tools.NewSkillLoadTool(c.activeSkills, c.cfg.Config().Options.SkillsPaths, c.cfg.WorkingDir(), c.skillTracker),
		tools.NewSourcegraphTool(nil),
		tools.NewTodosTool(c.sessions),
		tools.NewViewTool(c.lspManager, c.permissions, c.filetracker, c.skillTracker, c.cfg.WorkingDir(), c.cfg.Config().Options.SkillsPaths...),
		tools.NewWriteTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
	)
	if c.cfg.Config().Tools.CodebaseSearch.IsEnabled() {
		allTools = append(allTools, tools.NewCodebaseSearchTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.CodebaseSearch, nil))
	}
	if agent.Script != nil {
		allTools = append(allTools, tools.NewScriptTool(c.permissions, c.cfg.WorkingDir(), *agent.Script))
	}

	if !isSubAgent && c.interactive {
		allTools = append(
			allTools,
			tools.NewEnterPlanTool(c.sessions),
			tools.NewExitPlanTool(c.sessions, c.questions),
			tools.NewCompletePlanTool(c.sessions, c.questions),
			tools.NewQuestionTool(c.questions),
		)
	}

	// Add LSP tools if user has configured LSPs or auto_lsp is enabled (nil or true).
	if len(c.cfg.Config().LSP) > 0 || c.cfg.Config().Options.AutoLSP == nil || *c.cfg.Config().Options.AutoLSP {
		allTools = append(
			allTools,
			tools.NewDiagnosticsTool(c.lspManager),
			tools.NewReferencesTool(c.lspManager),
			tools.NewLSPRestartTool(c.lspManager),
			tools.NewSymbolsTool(c.lspManager),
			tools.NewDefinitionTool(c.lspManager),
			tools.NewCallHierarchyTool(c.lspManager),
			tools.NewRenameTool(c.lspManager, c.permissions, c.history, c.filetracker),
			tools.NewReplaceSymbolTool(c.lspManager, c.permissions, c.history, c.filetracker),
		)
	}

	if len(c.cfg.Config().MCP) > 0 {
		allTools = append(
			allTools,
			tools.NewListMCPResourcesTool(c.cfg, c.permissions),
			tools.NewReadMCPResourceTool(c.cfg, c.permissions),
		)
	}

	if agent.DefinitionPath != "" {
		available := make([]string, 0, len(allTools))
		for _, tool := range allTools {
			available = append(available, tool.Info().Name)
		}
		var disabled []string
		if c.cfg.Config().Options != nil {
			disabled = c.cfg.Config().Options.DisabledTools
		}
		resolvedTools, err := resolveCustomAgentTools(agent, available, disabled)
		if err != nil {
			return toolPalettes{}, err
		}
		agent.AllowedTools = resolvedTools
	}

	var filteredTools []fantasy.AgentTool
	for _, tool := range allTools {
		if slices.Contains(agent.AllowedTools, tool.Info().Name) {
			filteredTools = append(filteredTools, tool)
		}
	}

	for _, tool := range tools.GetMCPTools(c.permissions, c.cfg, c.cfg.WorkingDir()) {
		if agent.AllowedMCP == nil {
			// No MCP restrictions
			filteredTools = append(filteredTools, tool)
			continue
		}
		if len(agent.AllowedMCP) == 0 {
			// No MCPs allowed
			slog.Debug("No MCPs allowed", "tool", tool.Name(), "agent", agent.Name)
			break
		}

		for mcp, tools := range agent.AllowedMCP {
			if mcp != tool.MCP() {
				continue
			}
			if len(tools) == 0 || slices.Contains(tools, tool.MCPToolName()) {
				filteredTools = append(filteredTools, tool)
				break
			}
			slog.Debug("MCP not allowed", "tool", tool.Name(), "agent", agent.Name)
		}
	}
	planModeTools := make([]fantasy.AgentTool, 0, len(filteredTools))
	for _, tool := range filteredTools {
		if config.IsToolAllowedInPlanMode(tool.Info().Name) {
			planModeTools = append(planModeTools, tool)
		}
	}

	slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})
	slices.SortFunc(planModeTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})

	// Top-level tools always run hooks. Sub-agent wrappers activate only
	// during detached execution so foreground delegation remains unchanged.
	filteredTools = wrapToolsWithHooks(filteredTools, hookRunner, isSubAgent)
	planModeTools = wrapToolsWithHooks(planModeTools, hookRunner, isSubAgent)

	return toolPalettes{normal: filteredTools, planMode: planModeTools}, nil
}

func (c *coordinator) buildAgentModels(ctx context.Context, agent config.Agent, isSubAgent bool) (Model, Model, error) {
	var primaryModelCfg config.SelectedModel
	if agent.PrimaryModelOverride != nil {
		primaryModelCfg = *agent.PrimaryModelOverride
	} else {
		var ok bool
		primaryModelCfg, ok = c.cfg.Config().Models[agent.Model]
		if !ok {
			return Model{}, Model{}, errLargeModelNotSelected
		}
	}
	smallModelCfg, ok := c.cfg.Config().Models[config.SelectedModelTypeSmall]
	if !ok {
		return Model{}, Model{}, errSmallModelNotSelected
	}

	primaryProviderCfg, ok := c.cfg.Config().Providers.Get(primaryModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errLargeModelProviderNotConfigured
	}
	primaryProvider, err := c.buildProvider(primaryProviderCfg, primaryModelCfg, isSubAgent)
	if err != nil {
		return Model{}, Model{}, err
	}

	smallProviderCfg, ok := c.cfg.Config().Providers.Get(smallModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errSmallModelProviderNotConfigured
	}
	smallProvider, err := c.buildProvider(smallProviderCfg, smallModelCfg, true)
	if err != nil {
		return Model{}, Model{}, err
	}

	var primaryCatwalkModel *catwalk.Model
	var smallCatwalkModel *catwalk.Model
	for _, model := range primaryProviderCfg.Models {
		if model.ID == primaryModelCfg.Model {
			primaryCatwalkModel = &model
			break
		}
	}
	for _, model := range smallProviderCfg.Models {
		if model.ID == smallModelCfg.Model {
			smallCatwalkModel = &model
			break
		}
	}
	if primaryCatwalkModel == nil {
		return Model{}, Model{}, errLargeModelNotFound
	}
	if smallCatwalkModel == nil {
		return Model{}, Model{}, errSmallModelNotFound
	}

	primaryLanguageModel, err := primaryProvider.LanguageModel(ctx, primaryModelCfg.Model)
	if err != nil {
		return Model{}, Model{}, err
	}
	smallLanguageModel, err := smallProvider.LanguageModel(ctx, smallModelCfg.Model)
	if err != nil {
		return Model{}, Model{}, err
	}
	if registration, ok := config.ProviderCapabilities().Lookup(primaryProviderCfg.ID); ok && registration.ProviderID == primaryProviderCfg.ID {
		primaryLanguageModel = mapLanguageModelErrors(primaryLanguageModel, registration)
	}
	if registration, ok := config.ProviderCapabilities().Lookup(smallProviderCfg.ID); ok && registration.ProviderID == smallProviderCfg.ID {
		smallLanguageModel = mapLanguageModelErrors(smallLanguageModel, registration)
	}

	primary := Model{
		Model:              primaryLanguageModel,
		CatwalkCfg:         *primaryCatwalkModel,
		ModelCfg:           primaryModelCfg,
		FlatRate:           primaryProviderCfg.FlatRate,
		SystemPromptPrefix: primaryProviderCfg.SystemPromptPrefix,
		OnAuthRefresh:      c.makeAuthRefreshCallback(primaryProviderCfg),
	}
	primary.ProviderOptions = c.oauthReasoningOptions(
		primaryProviderCfg.ID,
		primaryModelCfg.Model,
		getProviderOptions(primary, primaryProviderCfg),
	)
	small := Model{
		Model:              smallLanguageModel,
		CatwalkCfg:         *smallCatwalkModel,
		ModelCfg:           smallModelCfg,
		FlatRate:           smallProviderCfg.FlatRate,
		SystemPromptPrefix: smallProviderCfg.SystemPromptPrefix,
		OnAuthRefresh:      c.makeAuthRefreshCallback(smallProviderCfg),
	}
	small.ProviderOptions = c.oauthReasoningOptions(
		smallProviderCfg.ID,
		smallModelCfg.Model,
		getProviderOptions(small, smallProviderCfg),
	)
	return primary, small, nil
}

func (c *coordinator) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []anthropic.Option{anthropic.WithAPIKey(apiKey)}
	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}
	if c.cfg.Config().Options.Debug {
		opts = append(opts, anthropic.WithHTTPClient(log.NewHTTPClient()))
	}
	return anthropic.New(opts...)
}

func (c *coordinator) buildManifestAnthropicProvider(registration providerregistry.Registration, baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	baseURL, err := anthropictransport.EffectiveBaseURL(registration.Operation, baseURL)
	if err != nil {
		return nil, err
	}
	httpClient, err := anthropictransport.NewClient(registration.Operation, c.cfg.Config().Options.Debug)
	if err != nil {
		return nil, err
	}
	headers["Authorization"] = "Bearer " + apiKey
	opts := []anthropic.Option{anthropic.WithHTTPClient(httpClient)}
	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}
	return anthropic.New(opts...)
}

func (c *coordinator) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}
	if c.cfg.Config().Options.Debug {
		httpClient := log.NewHTTPClient()
		opts = append(opts, openai.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openai.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

func (c *coordinator) buildOpenaiCompatProvider(baseURL, apiKey string, headers map[string]string, extraBody map[string]any, providerID string, isSubAgent bool) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(func(modelID string) bool {
				return copilotResponsesModels[modelID]
			}),
		)
		httpClient = copilotinference.NewClient(isSubAgent, c.cfg.Config().Options.Debug)
	}
	if httpClient == nil && c.cfg.Config().Options.Debug {
		httpClient = log.NewHTTPClient()
	}
	if httpClient != nil {
		opts = append(opts, openaicompat.WithHTTPClient(httpClient))
	}

	if len(headers) > 0 {
		opts = append(opts, openaicompat.WithHeaders(headers))
	}

	for extraKey, extraValue := range extraBody {
		opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
	}

	return openaicompat.New(opts...)
}

// buildCodexProvider creates the Codex provider: a native Responses-over-
// WebSocket adapter for the ChatGPT Codex endpoint, authenticating with an
// OAuth Bearer token and presenting the Codex CLI identity.
func (c *coordinator) buildCodexProvider(registration providerregistry.Registration, baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	accountID := func() string {
		if registration.AccountNamespace == "" {
			return ""
		}
		entry, err := accounts.Active(context.Background(), registration.AccountNamespace)
		if err != nil || entry == nil || entry.AccessToken != apiKey {
			return ""
		}
		var raw struct {
			AccountID string `json:"account_id"`
		}
		if err := json.Unmarshal(entry.Raw, &raw); err != nil {
			return ""
		}
		return raw.AccountID
	}
	return codex.NewProvider(baseURL, func() string { return apiKey }, accountID, headers, c.codexSessions)
}

// buildGeminiAntigravityProvider creates the Antigravity provider: a native
// Antigravity-dialect provider pointed at the Cloud Code v1internal endpoint,
// authenticating with an OAuth Bearer token.
func (c *coordinator) buildGeminiAntigravityProvider(baseURL, apiKey string, headers map[string]string) (fantasy.Provider, error) {
	return gemini.NewProvider(baseURL, func() string { return apiKey }, headers)
}

func (c *coordinator) buildProvider(providerCfg config.ProviderConfig, _ config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	registration, registered := c.cfg.ProviderRegistration(providerCfg.ID)
	if !registered && providerCfg.Plugin != nil {
		return nil, fmt.Errorf("provider %s is unavailable because plugin %s is not active; install, trust, enable, or select a compatible rollout profile for that plugin", providerCfg.ID, providerCfg.Plugin.ID)
	}
	if !registered && providerCfg.OAuthToken != nil && providerCfg.Type != catwalk.TypeOpenAICompat {
		return nil, fmt.Errorf("OAuth provider %s is unavailable because its registered integration is not active; install, trust, enable, or select the required provider plugin", providerCfg.ID)
	}

	apiKey, err := c.cfg.Resolve(providerCfg.APIKey)
	if err != nil {
		return nil, fmt.Errorf("resolve provider %s credential: %w", providerCfg.ID, err)
	}
	baseURL, err := c.cfg.Resolve(providerCfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("resolve provider %s endpoint: %w", providerCfg.ID, err)
	}
	if registered && registration.CompatibilityAdapter != "" && registration.Operation != nil {
		baseURL, err = registration.Operation.ResolveEndpoint(baseURL)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
		}
		headers, err = registration.Operation.ApplyHeaders(headers, nil)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
		}
	}

	if registered {
		switch registration.Construction {
		case providerregistry.ConstructionGeminiAntigravity:
			if registration.Operation != nil {
				if err := registration.Operation.ValidateSelection("gemini-generate-content", "sse"); err != nil {
					return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
				}
			}
			return c.buildGeminiAntigravityProvider(baseURL, apiKey, headers)
		case providerregistry.ConstructionCodex:
			if registration.Operation != nil {
				if err := registration.Operation.ValidateSelection("openai-responses", "websocket-json"); err != nil {
					return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
				}
			}
			return c.buildCodexProvider(registration, baseURL, apiKey, headers)
		case providerregistry.ConstructionCopilot:
			return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent)
		case providerregistry.ConstructionAnthropicMessages:
			if registration.Operation == nil {
				return nil, fmt.Errorf("provider %s has no operation contract", providerCfg.ID)
			}
			if err := registration.Operation.ValidateSelection(string(providerregistry.ConstructionAnthropicMessages), "sse"); err != nil {
				return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
			}
			if registration.Operation.Anthropic != nil {
				return c.buildManifestAnthropicProvider(registration, baseURL, apiKey, headers)
			}
			return c.buildAnthropicProvider(baseURL, apiKey, headers)
		case providerregistry.ConstructionOpenAIResponses:
			if registration.Operation == nil {
				return nil, fmt.Errorf("provider %s has no operation contract", providerCfg.ID)
			}
			if err := registration.Operation.ValidateSelection(string(providerregistry.ConstructionOpenAIResponses), "sse"); err != nil {
				return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
			}
			return c.buildOpenaiProvider(baseURL, apiKey, headers)
		case providerregistry.ConstructionGenericJSON:
			return nil, fmt.Errorf("provider %s requires a registered generic JSON transport", providerCfg.ID)
		default:
			return nil, fmt.Errorf("provider %s uses unsupported construction %q", providerCfg.ID, registration.Construction)
		}
	}

	if providerCfg.Type == openaicompat.Name {
		if providerCfg.ID == string(catwalk.InferenceProviderZAI) {
			if providerCfg.ExtraBody == nil {
				providerCfg.ExtraBody = map[string]any{}
			}
			providerCfg.ExtraBody["tool_stream"] = true
		}
		return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent)
	}
	if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
		return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent)
	}
	return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
}

// BeginAccepted reserves an accept slot for sessionID on the active
// agent and returns the ownership handle. It is the fire-and-forget
// dispatch path's only way to mark a run as accepted-but-not-yet-active
// so a cancel arriving before the run registers in activeRequests is not
// lost.
func (c *coordinator) BeginAccepted(sessionID string) *AcceptedRun {
	return c.currentAgent.BeginAccepted(sessionID)
}

func (c *coordinator) Cancel(sessionID string) {
	c.currentAgent.Cancel(sessionID)
}

func (c *coordinator) CancelAll() {
	c.currentAgent.CancelAll()
}

func (c *coordinator) CloseContext(ctx context.Context) {
	if c.backgroundAgents != nil {
		c.backgroundAgents.StopAll(ctx)
	}
	c.memoryWorker.Close()
	if c.codexSessions != nil {
		c.codexSessions.Close()
	}
}

func (c *coordinator) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), defaultBackgroundAgentStopTimeout)
	defer cancel()
	c.CloseContext(ctx)
}

func (c *coordinator) ClearQueue(sessionID string) {
	c.currentAgent.ClearQueue(sessionID)
}

func (c *coordinator) IsBusy() bool {
	return c.currentAgent.IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return c.currentAgent.IsSessionBusy(sessionID)
}

func (c *coordinator) Model() Model {
	return c.currentAgent.Model()
}

func (c *coordinator) UpdateModels(ctx context.Context) error {
	agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return errCoderAgentNotConfigured
	}

	large, small, err := c.buildAgentModels(ctx, agentCfg, false)
	if err != nil {
		return err
	}

	coder, err := coderPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return err
	}
	systemPrompt, err := coder.Build(ctx, large.ModelCfg.Provider, large.Model.Model(), c.cfg)
	if err != nil {
		return err
	}

	palettes, err := c.buildTools(ctx, agentCfg, false)
	if err != nil {
		return err
	}

	c.currentAgent.SetModels(large, small)
	c.currentAgent.SetSystemPrompt(systemPrompt)
	c.currentAgent.SetTools(palettes.normal, palettes.planMode)
	c.systemPromptTemplate = coder
	return nil
}

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent.QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []QueuedPrompt {
	return c.currentAgent.QueuedPromptsList(sessionID)
}

func (c *coordinator) Summarize(ctx context.Context, sessionID string) error {
	providerCfg, ok := c.cfg.Config().Providers.Get(c.currentAgent.Model().ModelCfg.Provider)
	if !ok {
		return errModelProviderNotConfigured
	}

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		slog.Error("Failed to refresh OAuth2 token before summarize. Proceeding with existing token.", "provider", providerCfg.ID)
	}

	// Auth failures during summarize flow through fantasy's OnAuthRefresh,
	// the same path used by regular turns.
	model := c.currentAgent.Model()
	providerOptions := c.oauthReasoningOptions(providerCfg.ID, model.ModelCfg.Model, getProviderOptions(model, providerCfg))
	err := c.currentAgent.Summarize(ctx, sessionID, providerOptions, c.makeAuthRefreshCallback(providerCfg))
	if err != nil {
		c.disableOAuthReasoning(providerCfg.ID, model.ModelCfg.Model, err.Error())
	}
	return err
}

// GenerateTitle generates a session title using the current agent.
func (c *coordinator) GenerateTitle(ctx context.Context, sessionID, prompt string) {
	if c.currentAgent == nil {
		return
	}
	c.currentAgent.GenerateTitle(ctx, sessionID, prompt)
}

// SuggestPrompt predicts the user's likely next message using the
// small model.
func (c *coordinator) SuggestPrompt(ctx context.Context, sessionID string) (string, error) {
	if c.currentAgent == nil {
		return "", errors.New("agent not initialized")
	}
	return c.currentAgent.SuggestPrompt(ctx, sessionID)
}

// refreshTokenIfExpired proactively refreshes the OAuth token if it has expired.
func (c *coordinator) refreshTokenIfExpired(ctx context.Context, providerCfg config.ProviderConfig) error {
	if providerCfg.OAuthToken == nil || !providerCfg.OAuthToken.IsExpired() {
		return nil
	}
	slog.Debug("Token needs to be refreshed", "provider", providerCfg.ID)
	return c.refreshOAuth2Token(ctx, providerCfg)
}

// retryAfterUnauthorized attempts to refresh credentials after an auth error
// and returns nil if the request should be retried. For OAuth providers whose
// refresh token is revoked, it triggers interactive re-authentication and
// blocks until the user completes it (or the context is cancelled).
func (c *coordinator) retryAfterUnauthorized(ctx context.Context, providerCfg config.ProviderConfig) error {
	switch {
	case providerCfg.OAuthToken != nil:
		slog.Debug("Received 401. Refreshing token and retrying", "provider", providerCfg.ID)
		if err := c.refreshOAuth2Token(ctx, providerCfg); err != nil {
			// If the refresh token was revoked, trigger interactive
			// re-auth and wait for the user to complete it.
			var exchangeErr *oauth.TokenExchangeError
			if c.notify != nil && errors.As(err, &exchangeErr) && exchangeErr.IsRefreshTokenRevoked() {
				slog.Info("Refresh token revoked, waiting for re-authentication", "provider", providerCfg.ID)
				c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
					Type:       notify.TypeReAuthenticate,
					ProviderID: providerCfg.ID,
				})
				return c.waitForInteractiveReauth(ctx, providerCfg.ID)
			}
			return err
		}
		return nil
	case strings.Contains(providerCfg.APIKeyTemplate, "$"):
		slog.Debug("Received 401. Refreshing API Key template and retrying", "provider", providerCfg.ID)
		return c.refreshApiKeyTemplate(ctx, providerCfg)
	default:
		return nil
	}
}

// errNoInteractiveAuth is returned by an OnAuthRefresh callback when a
// provider needs interactive re-authentication but no notifier is available
// to drive it (e.g. headless runs). Returning it surfaces the original auth
// error rather than retrying.
var errNoInteractiveAuth = errors.New("interactive authentication unavailable")

// waitForInteractiveReauth blocks until interactive re-authentication for the
// provider completes (signalled via SignalAuthComplete) or the context is
// cancelled, then rebuilds models so the next attempt picks up fresh
// credentials. Returns nil when the caller should retry.
func (c *coordinator) waitForInteractiveReauth(ctx context.Context, providerID string) error {
	// Use a detached context with a generous timeout so the wait survives
	// agent run cancellation. The user needs time to complete browser-based
	// authentication.
	waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer waitCancel()
	slog.Info("Blocking on WaitForTokenChange", "provider", providerID)
	if waitErr := c.cfg.WaitForTokenChange(waitCtx, providerID); waitErr != nil {
		slog.Info("WaitForTokenChange returned error", "provider", providerID, "error", waitErr)
		return waitErr
	}
	// If the original context was cancelled during the wait, fantasy's retry
	// would fail immediately, so surface the cancellation instead.
	if ctx.Err() != nil {
		slog.Warn("Original context cancelled during auth wait, cannot retry",
			"provider", providerID, "ctx_err", ctx.Err())
		return ctx.Err()
	}
	// Rebuild models so ModelProvider picks up the fresh credentials.
	if updateErr := c.UpdateModels(waitCtx); updateErr != nil {
		slog.Error("Failed to update models after re-authentication", "provider", providerID)
		return updateErr
	}
	slog.Info("Models updated, returning nil to retry", "provider", providerID)
	return nil
}

// isUnauthorized reports whether err is an HTTP 401 from a provider.
func isUnauthorized(err error) bool {
	var providerErr *fantasy.ProviderError
	return errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized
}

// makeAuthRefreshCallback returns an OnAuthRefresh callback for fantasy that
// delegates to the coordinator's existing credential refresh logic. Returns
// nil if no refresh mechanism is configured for the provider.
func (c *coordinator) makeAuthRefreshCallback(providerCfg config.ProviderConfig) func(context.Context, *fantasy.ProviderError) error {
	if providerCfg.OAuthToken == nil &&
		!strings.Contains(providerCfg.APIKeyTemplate, "$") {
		return nil
	}
	return func(ctx context.Context, _ *fantasy.ProviderError) error {
		return c.retryAfterUnauthorized(ctx, providerCfg)
	}
}

func (c *coordinator) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig) error {
	if err := c.cfg.RefreshOAuthToken(ctx, config.ScopeGlobal, providerCfg.ID); err != nil {
		slog.Error("Failed to refresh OAuth token after 401 error", "provider", providerCfg.ID)
		return err
	}
	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

func (c *coordinator) refreshApiKeyTemplate(ctx context.Context, providerCfg config.ProviderConfig) error {
	newAPIKey, err := c.cfg.Resolve(providerCfg.APIKeyTemplate)
	if err != nil {
		slog.Error("Failed to re-resolve API key after 401 error", "provider", providerCfg.ID)
		return err
	}

	providerCfg.APIKey = newAPIKey
	c.cfg.Config().Providers.Set(providerCfg.ID, providerCfg)

	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return nil
}

// subAgentParams holds the parameters for running a sub-agent.
type subAgentParams struct {
	Agent          SessionAgent
	SessionID      string
	AgentMessageID string
	ToolCallID     string
	Prompt         string
	SessionTitle   string
	ChildSessionID string
	// SessionSetup is an optional callback invoked after session creation
	// but before agent execution, for custom session configuration.
	SessionSetup func(sessionID string)
}

// runSubAgent runs a sub-agent and handles session management and cost accumulation.
// It creates a sub-session, runs the agent with the given prompt, and propagates
// the cost to the parent session.
func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	var session session.Session
	var err error
	if params.ChildSessionID == "" {
		agentToolSessionID := c.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
		session, err = c.sessions.CreateTaskSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
		}
	} else {
		session, err = c.sessions.Get(ctx, params.ChildSessionID)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("get child session: %w", err)
		}
	}

	// Call session setup function if provided
	if params.SessionSetup != nil {
		params.SessionSetup(session.ID)
	}

	// Get model configuration
	model := params.Agent.Model()
	maxTokens := model.CatwalkCfg.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return fantasy.ToolResponse{}, errModelProviderNotConfigured
	}
	providerOptions := c.oauthReasoningOptions(providerCfg.ID, model.ModelCfg.Model, getProviderOptions(model, providerCfg))

	// Run the agent
	run := func() (*fantasy.AgentResult, error) {
		return params.Agent.Run(ctx, SessionAgentCall{
			SessionID:        session.ID,
			Prompt:           params.Prompt,
			MaxOutputTokens:  maxTokens,
			ProviderOptions:  providerOptions,
			Temperature:      model.ModelCfg.Temperature,
			TopP:             model.ModelCfg.TopP,
			TopK:             model.ModelCfg.TopK,
			FrequencyPenalty: model.ModelCfg.FrequencyPenalty,
			PresencePenalty:  model.ModelCfg.PresencePenalty,
			NonInteractive:   true,
			OnProviderWarning: func(w fantasy.CallWarning) {
				c.disableOAuthReasoning(providerCfg.ID, model.ModelCfg.Model, w.Message)
			},
			OnAuthRefresh: c.makeAuthRefreshCallback(providerCfg),
		})
	}
	initialChildCost := session.Cost
	result, err := run()
	if err != nil {
		c.disableOAuthReasoning(providerCfg.ID, model.ModelCfg.Model, err.Error())
	}
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to generate response: %s", err)), nil
	}

	// Update parent session cost on a best-effort basis. A failure here must
	// not discard the sub-agent output that was already produced.
	if err := c.updateParentSessionCost(ctx, session.ID, params.SessionID, initialChildCost); err != nil {
		slog.Warn(
			"Failed to update parent session cost",
			"child_session", session.ID,
			"parent_session", params.SessionID,
			"error", err,
		)
	}

	output := subAgentOutput(result)
	if output == "" {
		return fantasy.NewTextErrorResponse("Sub-agent completed but produced no text output."), nil
	}
	return fantasy.NewTextResponse(output), nil
}

func subAgentOutput(result *fantasy.AgentResult) string {
	if result == nil {
		return ""
	}
	return result.Response.Content.Text()
}

// updateParentSessionCost accumulates the cost from a child session to its parent session.
func (c *coordinator) updateParentSessionCost(ctx context.Context, childSessionID, parentSessionID string, initialChildCost float64) error {
	childSession, err := c.sessions.Get(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("get child session: %w", err)
	}

	parentSession, err := c.sessions.Get(ctx, parentSessionID)
	if err != nil {
		return fmt.Errorf("get parent session: %w", err)
	}

	parentSession.Cost += max(childSession.Cost-initialChildCost, 0)

	if _, err := c.sessions.Save(ctx, parentSession); err != nil {
		return fmt.Errorf("save parent session: %w", err)
	}

	return nil
}

// discoverSkills is a thin fallback wrapper used only when no
// skills.Manager has been threaded through to the coordinator. All
// production call sites (backend.CreateWorkspace, setupLocalWorkspace)
// run discovery in advance and pass the results via the manager;
// reaching this path means a caller bypassed both. It deliberately does
// NOT publish to the package-level broker — there are no subscribers in
// that case, so doing so would be misleading without delivering the
// snapshot anywhere useful.
func discoverSkills(cfg *config.ConfigStore) (allSkills, activeSkills []*skills.Skill) {
	opts := cfg.Config().Options
	var paths, disabled []string
	if opts != nil {
		paths = opts.SkillsPaths
		disabled = opts.DisabledSkills
	}
	var resolver func(string) (string, error)
	if r := cfg.Resolver(); r != nil {
		resolver = r.ResolveValue
	}
	allSkills, activeSkills, states := skills.DiscoverFromConfig(skills.DiscoveryConfig{
		SkillsPaths:    paths,
		DisabledSkills: disabled,
		WorkingDir:     cfg.WorkingDir(),
		Resolver:       resolver,
	})
	logDiscoveryStats(states, paths, allSkills, activeSkills, disabled)
	return allSkills, activeSkills
}

// logTurnSkillUsage emits a per-turn diagnostic line showing which skills
// (if any) were loaded during this turn and which looked relevant based on
// a cheap keyword match against the user prompt. The goal is to surface
// "should-have-loaded but didn't" situations for later analysis.
//
// Logged at Info level under component=skills; heavy fields are elided when
// there is nothing interesting to report.
func logTurnSkillUsage(
	sessionID string,
	prompt string,
	activeSkills []*skills.Skill,
	tracker *skills.Tracker,
	before []string,
) {
	if tracker == nil || len(activeSkills) == 0 {
		return
	}

	after := tracker.LoadedNames()

	beforeSet := make(map[string]bool, len(before))
	for _, n := range before {
		beforeSet[n] = true
	}
	var loadedThisTurn []string
	for _, n := range after {
		if !beforeSet[n] {
			loadedThisTurn = append(loadedThisTurn, n)
		}
	}

	slog.Info(
		"Skill turn summary",
		"component", "skills",
		"session_id", sessionID,
		"prompt_len", len(prompt),
		"active_total", len(activeSkills),
		"loaded_total", len(after),
		"loaded_this_turn", loadedThisTurn,
	)
}

// logDiscoveryStats emits a single structured log line summarising skill
// discovery for the current session. It is intentionally low-volume: one
// line per session start. Builtin vs user counts are derived from the
// SkillState.Path — builtin states use the "builtin/" embed prefix.
func logDiscoveryStats(
	states []*skills.SkillState,
	userPaths []string,
	allSkills, activeSkills []*skills.Skill,
	disabled []string,
) {
	var builtinOK, builtinErr, userOK, userErr int
	for _, s := range states {
		isBuiltin := strings.HasPrefix(s.Path, "builtin/")
		switch {
		case isBuiltin && s.State == skills.StateNormal:
			builtinOK++
		case isBuiltin && s.State == skills.StateError:
			builtinErr++
		case !isBuiltin && s.State == skills.StateNormal:
			userOK++
		case !isBuiltin && s.State == skills.StateError:
			userErr++
		}
	}

	activeNames := make([]string, 0, len(activeSkills))
	for _, s := range activeSkills {
		activeNames = append(activeNames, s.Name)
	}

	xml := skills.ToPromptXML(activeSkills)

	slog.Info(
		"Skill discovery complete",
		"component", "skills",
		"builtin_ok", builtinOK,
		"builtin_errors", builtinErr,
		"user_ok", userOK,
		"user_errors", userErr,
		"user_paths", len(userPaths),
		"deduped_total", len(allSkills),
		"active", len(activeSkills),
		"disabled", len(disabled),
		"prompt_bytes", len(xml),
		"prompt_tok_est", skills.ApproxTokenCount(xml),
		"active_names", activeNames,
	)
}
