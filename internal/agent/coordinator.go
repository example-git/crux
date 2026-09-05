package agent

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
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

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
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
	"github.com/example-git/crux/internal/imagegen"
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
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	anthropictransport "github.com/example-git/crux/internal/providertransport/anthropic"
	declarativetransport "github.com/example-git/crux/internal/providertransport/declarative"
	openairesponsestransport "github.com/example-git/crux/internal/providertransport/openairesponses"
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
const (
	codebaseIndexReconcileInterval        = time.Minute
	codebaseIndexReconcileRequestInterval = 30 * time.Second
)

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

type nativeResponsesContinuationProvider interface {
	fantasy.Provider
	continuationOwner() string
}

type nativeResponsesProvider struct {
	fantasy.Provider
	owner string
}

func (p *nativeResponsesProvider) continuationOwner() string {
	return p.owner
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
	UpdateModelsForState(ctx context.Context, expected config.AgentModelState) error
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
	backgroundImages         *imagegen.JobManager

	currentAgent         SessionAgent
	systemPromptTemplate *prompt.Prompt
	agents               map[string]SessionAgent

	// Skills discovery results. skillsManager is the authoritative workspace
	// generation; the local fields are the exact generation installed into the
	// current prompt and skill tools.
	skillsManager *skills.Manager
	allSkills     []*skills.Skill
	activeSkills  []*skills.Skill
	skillTracker  *skills.Tracker
	skillsMu      sync.RWMutex
	updateMu      sync.Mutex

	readyWg errgroup.Group

	reasoningMu            sync.RWMutex
	reasoningDisabled      map[string]bool
	codexSessions          *codexresponses.SessionStore
	responsesContinuations *openairesponsestransport.ContinuationStore
	memoryWorker           *automemory.Worker

	codebaseIndexReconcileMu     sync.Mutex
	codebaseIndexReconciling     bool
	codebaseIndexLastReconcile   time.Time
	codebaseIndexLifecycleCtx    context.Context
	codebaseIndexLifecycleCancel context.CancelFunc
	codebaseIndexLifecycleDone   <-chan struct{}
	reconcileCodebaseIndexFn     func(context.Context) (codebaseindex.StoreStatus, error)
	generationBoundary           func()
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
	BackgroundImages *imagegen.JobManager
}

func NewCoordinator(ctx context.Context, opts CoordinatorOptions) (Coordinator, error) {
	if opts.BackgroundShells == nil {
		opts.BackgroundShells = shell.NewBackgroundShellManager(opts.Config.WorkingDir())
	}
	if opts.BackgroundAgents == nil {
		var err error
		opts.BackgroundAgents, err = NewBackgroundAgentManagerWithStore(opts.Config.WorkingDir(), opts.BackgroundShells, nil)
		if err != nil {
			return nil, fmt.Errorf("initialize global background agent admission: %w", err)
		}
	}
	if opts.BackgroundImages == nil {
		opts.BackgroundImages = imagegen.NewJobManager(opts.Config.WorkingDir())
	}

	agentCfg, ok := opts.Config.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	// Skills are pre-discovered by the caller (see app.New /
	// backend.CreateWorkspace) and passed in via the manager. If no
	// manager was provided (legacy callers), fall back to an in-line
	// discovery so the coordinator still works.
	var snapshot skills.Snapshot
	if opts.Skills != nil {
		snapshot = opts.Skills.Snapshot()
	} else {
		snapshot = discoverSkillSnapshot(opts.Config)
	}
	snapshot = providerSkillSnapshot(snapshot, opts.Config.Config(), selectedAgentProvider(opts.Config.Config(), agentCfg))
	allSkills, activeSkills := snapshot.AllSkills, snapshot.ActiveSkills
	skillTracker := skills.NewTracker(activeSkills)

	c := &coordinator{
		cfg:                    opts.Config,
		sessions:               opts.Sessions,
		messages:               opts.Messages,
		permissions:            opts.Permissions,
		questions:              opts.Questions,
		history:                opts.History,
		filetracker:            opts.FileTracker,
		lspManager:             opts.LSPManager,
		notify:                 opts.Notify,
		runComplete:            opts.RunComplete,
		agents:                 make(map[string]SessionAgent),
		skillsManager:          opts.Skills,
		allSkills:              allSkills,
		activeSkills:           activeSkills,
		skillTracker:           skillTracker,
		interactive:            opts.Interactive,
		reasoningDisabled:      make(map[string]bool),
		codexSessions:          codexresponses.NewSessionStore(),
		responsesContinuations: openairesponsestransport.NewContinuationStore(),
		loadAgentDefinitions:   discoverAgentDefinitions,
		backgroundShells:       opts.BackgroundShells,
		backgroundAgents:       opts.BackgroundAgents,
		backgroundImages:       opts.BackgroundImages,
	}
	c.automaticCodebaseContext = c.retrieveAutomaticCodebaseContext

	// TODO: make this dynamic when we support multiple agents
	prompt, err := coderPrompt(
		prompt.WithWorkingDir(c.cfg.WorkingDir()),
		prompt.WithSkills(activeSkills),
	)
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
	c.startCodebaseIndexLifecycle(ctx, codebaseIndexReconcileInterval)
	c.cfg.SetRuntimeGenerationPreparer(c.prepareRuntimeGeneration)
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

func (c *coordinator) startCodebaseIndexLifecycle(ctx context.Context, interval time.Duration) {
	lifecycleCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	c.codebaseIndexReconcileMu.Lock()
	previousCancel := c.codebaseIndexLifecycleCancel
	c.codebaseIndexLifecycleCtx = lifecycleCtx
	c.codebaseIndexLifecycleCancel = cancel
	c.codebaseIndexLifecycleDone = done
	c.codebaseIndexReconcileMu.Unlock()
	if previousCancel != nil {
		previousCancel()
	}

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-lifecycleCtx.Done():
				return
			case <-ticker.C:
				c.scheduleCodebaseIndexReconcile()
			}
		}
	}()
	c.scheduleCodebaseIndexReconcile()
}

func (c *coordinator) stopCodebaseIndexLifecycle(ctx context.Context) {
	c.codebaseIndexReconcileMu.Lock()
	cancel := c.codebaseIndexLifecycleCancel
	done := c.codebaseIndexLifecycleDone
	c.codebaseIndexLifecycleCancel = nil
	c.codebaseIndexLifecycleDone = nil
	c.codebaseIndexLifecycleCtx = nil
	c.codebaseIndexReconcileMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (c *coordinator) requestCodebaseIndexReconcile() {
	c.requestCodebaseIndexReconcileAfter(codebaseIndexReconcileRequestInterval)
}

func (c *coordinator) requestCodebaseIndexReconcileAfter(interval time.Duration) {
	c.codebaseIndexReconcileMu.Lock()
	lifecycleCtx := c.codebaseIndexLifecycleCtx
	if lifecycleCtx == nil || lifecycleCtx.Err() != nil || c.codebaseIndexReconciling || time.Since(c.codebaseIndexLastReconcile) < interval {
		c.codebaseIndexReconcileMu.Unlock()
		return
	}
	c.codebaseIndexReconcileMu.Unlock()
	c.scheduleCodebaseIndexReconcile()
}

func (c *coordinator) scheduleCodebaseIndexReconcile() {
	c.codebaseIndexReconcileMu.Lock()
	if c.codebaseIndexReconciling {
		c.codebaseIndexReconcileMu.Unlock()
		return
	}
	ctx := c.codebaseIndexLifecycleCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		c.codebaseIndexReconcileMu.Unlock()
		return
	}
	c.codebaseIndexReconciling = true
	c.codebaseIndexLastReconcile = time.Now()
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
		if _, err := reconcile(ctx); err != nil && ctx.Err() == nil {
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

type InstructionSnapshotSection struct {
	Kind          fantasy.InstructionKind      `json:"kind"`
	Stability     fantasy.InstructionStability `json:"stability"`
	Text          string                       `json:"text"`
	CacheBoundary bool                         `json:"cache_boundary,omitempty"`
}

type InstructionSnapshot struct {
	ProviderID string                       `json:"provider_id"`
	ModelID    string                       `json:"model_id"`
	Policy     fantasy.InstructionPolicy    `json:"policy"`
	Sections   []InstructionSnapshotSection `json:"sections"`
}

type instructionSnapshotController interface {
	instructionSnapshot(context.Context) (InstructionSnapshot, error)
}

func CurrentInstructionSnapshot(ctx context.Context, coordinator Coordinator) (InstructionSnapshot, error) {
	controller, ok := coordinator.(instructionSnapshotController)
	if !ok {
		return InstructionSnapshot{}, fmt.Errorf("instruction preview is unavailable")
	}
	return controller.instructionSnapshot(ctx)
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

	runtime := c.currentAgent.Runtime()
	model := runtime.LargeModel
	maxTokens := model.CatalogModel.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}

	cfg := runtime.Snapshot.Config()
	providerCfg, ok := cfg.Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}
	providerCfg.ID = model.ModelCfg.Provider

	registration, registered := runtime.Snapshot.ProviderBehaviorRegistration(model.ModelCfg.Provider, providerCfg)
	mergedOptions, temp, topP, topK, freqPenalty, presPenalty, err := mergeCallOptions(model, providerCfg, registration)
	if err != nil {
		return nil, fmt.Errorf("provider %s options: %w", model.ModelCfg.Provider, err)
	}
	mergedOptions = c.oauthReasoningOptionsForRegistration(model.ModelCfg.Provider, model.ModelCfg.Model, registration, registered, mergedOptions)
	mergedOptions = applyRegisteredRuntimeOptions(model.ModelCfg.Model, cfg.Options, registration, registered, mergedOptions)

	if err := c.refreshTokenIfExpired(ctx, runtime.Snapshot, providerCfg); err != nil {
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
	codebaseInstructions := codebaseContext.wait()
	memoryInstructions, memoryErr := automemory.Relevant(ctx, c.cfg.WorkingDir(), prompt, time.Now())
	if memoryErr != nil {
		slog.Debug("Could not load relevant auto-memory", "error", memoryErr)
		memoryInstructions = ""
	}
	run := func() (*fantasy.AgentResult, error) {
		return c.currentAgent.Run(ctx, SessionAgentCall{
			SessionID:            sessionID,
			SubmissionID:         submissionID,
			RunID:                runID,
			Prompt:               prompt,
			CodebaseInstructions: codebaseInstructions,
			MemoryInstructions:   memoryInstructions,
			Attachments:          attachments,
			MaxOutputTokens:      maxTokens,
			ProviderOptions:      mergedOptions,
			Temperature:          temp,
			TopP:                 topP,
			TopK:                 topK,
			FrequencyPenalty:     freqPenalty,
			PresencePenalty:      presPenalty,
			OnComplete:           onComplete,
			OnProviderWarning: func(w fantasy.CallWarning) {
				c.disableOAuthReasoningForRegistration(model.ModelCfg.Provider, model.ModelCfg.Model, registration, registered, w.Message)
			},
			Accepted:      accept,
			OnAuthRefresh: c.makeAuthRefreshCallback(runtime.Snapshot, providerCfg),
			runtime:       &runtime,
		})
	}
	c.skillsMu.RLock()
	turnSkills := append([]*skills.Skill(nil), c.activeSkills...)
	turnTracker := c.skillTracker
	c.skillsMu.RUnlock()
	beforeLoaded := turnTracker.LoadedNames()
	result, originalErr := run()
	if originalErr != nil {
		c.disableOAuthReasoningForRegistration(model.ModelCfg.Provider, model.ModelCfg.Model, registration, registered, originalErr.Error())
	}
	logTurnSkillUsage(sessionID, prompt, turnSkills, turnTracker, beforeLoaded)
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
	if !model.CatalogModel.CanReason {
		return ""
	}

	if effort := model.ModelCfg.ReasoningEffort; effort != "" && slices.Contains(model.CatalogModel.ReasoningLevels, effort) {
		return effort
	}
	if effort := model.CatalogModel.DefaultReasoningEffort; effort != "" && slices.Contains(model.CatalogModel.ReasoningLevels, effort) {
		return effort
	}
	if len(model.CatalogModel.ReasoningLevels) > 0 {
		return model.CatalogModel.ReasoningLevels[0]
	}
	return ""
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

func (c *coordinator) disableOAuthReasoningForRegistration(providerID, modelID string, registration providerregistry.Registration, registered bool, message string) {
	if !registered || registration.Reasoning == nil || !registration.Reasoning.FallbackOnUnsupported || !isUnsupportedReasoningMessage(message) {
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

func (c *coordinator) oauthReasoningOptionsForRegistration(providerID, modelID string, registration providerregistry.Registration, registered bool, options fantasy.ProviderOptions) fantasy.ProviderOptions {
	if !registered || registration.Reasoning == nil {
		return options
	}
	return c.oauthReasoningOptionsForCapability(providerID, modelID, registration.Reasoning, options)
}

func (c *coordinator) oauthReasoningOptionsForCapability(providerID, modelID string, reasoning *providerregistry.ReasoningCapability, options fantasy.ProviderOptions) fantasy.ProviderOptions {
	if reasoning.Disable == nil {
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

// applyRegisteredRuntimeOptions is the final declarative runtime-control hook
// for registered providers. Keep the exact provider and model availability
// checks: applying another registration's controls can silently change request
// semantics, while bypassing this hook drops manifest-exposed user settings.
func applyRegisteredRuntimeOptions(modelID string, cfg *config.Options, registration providerregistry.Registration, registered bool, options fantasy.ProviderOptions) fantasy.ProviderOptions {
	if !registered || registration.Runtime == nil ||
		registration.Runtime.Available == nil || !registration.Runtime.Available(modelID) ||
		registration.Runtime.Apply == nil || cfg == nil {
		return options
	}
	return registration.Runtime.Apply(providerregistry.RuntimeValues{
		ResponseVerbosity: cfg.ResponseVerbosity,
		AnalysisEffort:    cfg.AnalysisEffort,
	}, options)
}

func legacyProviderBehaviorID(providerID string, providerCfg config.ProviderConfig) string {
	if providerCfg.Plugin != nil || providerCfg.Preset != nil {
		return ""
	}
	return providerID
}

// getProviderOptions preserves the provider-option precedence selected by the
// user: catalog defaults, then provider configuration, then selected-model
// overrides. Plugin-backed models rely on this function to carry their custom
// values into requests. Do not discard the selected-model layer or replace it
// with options from an available default provider.
func getProviderOptions(model Model, providerCfg config.ProviderConfig, registration providerregistry.Registration) (fantasy.ProviderOptions, error) {
	options := fantasy.ProviderOptions{}

	cfgOpts := []byte("{}")
	providerCfgOpts := []byte("{}")
	catalogOptions := []byte("{}")

	if model.ModelCfg.ProviderOptions != nil {
		data, err := json.Marshal(model.ModelCfg.ProviderOptions)
		if err != nil {
			return nil, fmt.Errorf("serialize selected-model provider options: %w", err)
		}
		cfgOpts = data
	}
	if providerCfg.ProviderOptions != nil {
		data, err := json.Marshal(providerCfg.ProviderOptions)
		if err != nil {
			return nil, fmt.Errorf("serialize provider options: %w", err)
		}
		providerCfgOpts = data
	}
	if model.CatalogModel.Options.ProviderOptions != nil {
		data, err := json.Marshal(model.CatalogModel.Options.ProviderOptions)
		if err != nil {
			return nil, fmt.Errorf("serialize catalog provider options: %w", err)
		}
		catalogOptions = data
	}

	got, err := jsons.Merge([]io.Reader{
		bytes.NewReader(catalogOptions),
		bytes.NewReader(providerCfgOpts),
		bytes.NewReader(cfgOpts),
	})
	if err != nil {
		return nil, fmt.Errorf("merge provider options: %w", err)
	}

	mergedOptions := make(map[string]any)
	if err := json.Unmarshal([]byte(got), &mergedOptions); err != nil {
		return nil, fmt.Errorf("decode merged provider options: %w", err)
	}

	reasoningEffort := effectiveReasoningEffort(model)
	providerID := model.ModelCfg.Provider
	legacyProviderID := legacyProviderBehaviorID(providerID, providerCfg)
	registered := registration.ProviderID != ""
	if registered && registration.Reasoning != nil && registration.Reasoning.Options != nil {
		return registration.Reasoning.Options(model.CatalogModel.ID, reasoningEffort, model.CatalogModel.CanReason, mergedOptions)
	}

	if registered && registration.Construction == providerregistry.ConstructionAnthropicMessages {
		shouldSetEffort := model.CatalogModel.CanReason && reasoningEffort != "" && slices.Contains(model.CatalogModel.ReasoningLevels, reasoningEffort)
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
		parsed, err := anthropic.ParseOptions(mergedOptions)
		if err != nil {
			return nil, fmt.Errorf("parse Anthropic provider options: %w", err)
		}
		options[anthropic.Name] = parsed
		return options, nil
	}

	if registered && registration.Construction == providerregistry.ConstructionOpenAIResponses {
		if openai.IsResponsesReasoningModel(model.CatalogModel.ID) {
			if _, exists := mergedOptions["reasoning_effort"]; !exists && reasoningEffort != "" {
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
			mergedOptions["reasoning_summary"] = "auto"
			mergedOptions["include"] = []openai.IncludeType{openai.IncludeReasoningEncryptedContent}
		}
		parsed, err := openai.ParseResponsesOptions(mergedOptions)
		if err != nil {
			return nil, fmt.Errorf("parse OpenAI Responses provider options: %w", err)
		}
		options[openai.Name] = parsed
		return options, nil
	}

	if providerCfg.Type != openaicompat.Name && !discover.IsKnownCustomProvider(string(providerCfg.Type)) {
		return options, nil
	}

	extraBody := make(map[string]any)
	shouldSetEffort := model.CatalogModel.CanReason && reasoningEffort != "" && slices.Contains(model.CatalogModel.ReasoningLevels, reasoningEffort)
	if _, exists := mergedOptions["reasoning_effort"]; !exists && shouldSetEffort {
		switch legacyProviderID {
		case string(catalog.ProviderIoNet):
			extraBody["reasoning"] = map[string]string{"effort": reasoningEffort}
		case string(catalog.ProviderOpenCodeGo), string(catalog.ProviderOpenCodeZen):
			if !strings.HasPrefix(strings.ToLower(model.CatalogModel.ID), "minimax") {
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
		default:
			mergedOptions["reasoning_effort"] = reasoningEffort
		}
	}

	switch legacyProviderID {
	case string(catalog.ProviderIoNet):
		if _, exists := extraBody["reasoning"]; !exists && model.CatalogModel.CanReason {
			if model.ModelCfg.Think {
				extraBody["reasoning"] = map[string]string{"effort": "medium"}
			} else {
				extraBody["reasoning"] = map[string]string{"effort": "none"}
			}
		}
	case string(catalog.ProviderZAI), string(catalog.ProviderDeepSeek):
		if model.ModelCfg.Think || reasoningEffort != "" {
			extraBody["thinking"] = map[string]any{"type": "enabled"}
		} else {
			extraBody["thinking"] = map[string]any{"type": "disabled"}
		}
	case string(catalog.ProviderFireworks):
		if reasoningEffort == "" {
			if model.ModelCfg.Think {
				extraBody["thinking"] = map[string]any{"type": "enabled"}
			} else {
				extraBody["thinking"] = map[string]any{"type": "disabled"}
			}
		}
	case string(catalog.ProviderBaseten):
		extraBody["chat_template_args"] = map[string]any{"enable_thinking": model.ModelCfg.Think || reasoningEffort != "" && reasoningEffort != "none"}
	case string(catalog.ProviderOpenCodeGo), string(catalog.ProviderOpenCodeZen):
		if strings.HasPrefix(strings.ToLower(model.CatalogModel.ID), "minimax") {
			if model.CatalogModel.CanReason && (model.ModelCfg.Think || reasoningEffort != "") {
				extraBody["thinking"] = map[string]any{"type": "adaptive"}
				extraBody["reasoning_split"] = true
			} else {
				extraBody["thinking"] = map[string]any{"type": "disabled"}
			}
		}
	case string(catalog.ProviderAlibabaSingapore), string(catalog.ProviderAlibabaUS):
		if model.CatalogModel.CanReason {
			extraBody["enable_thinking"] = model.ModelCfg.Think || reasoningEffort != ""
		}
	}

	mergedOptions["extra_body"] = extraBody
	parsed, err := openaicompat.ParseOptions(mergedOptions)
	if err != nil {
		return nil, fmt.Errorf("parse OpenAI-compatible provider options: %w", err)
	}
	options[openaicompat.Name] = parsed
	return options, nil
}

func mergeCallOptions(model Model, cfg config.ProviderConfig, registration providerregistry.Registration) (fantasy.ProviderOptions, *float64, *float64, *int64, *float64, *float64, error) {
	modelOptions, err := getProviderOptions(model, cfg, registration)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	temp := cmp.Or(model.ModelCfg.Temperature, model.CatalogModel.Options.Temperature)
	topP := cmp.Or(model.ModelCfg.TopP, model.CatalogModel.Options.TopP)
	topK := cmp.Or(model.ModelCfg.TopK, model.CatalogModel.Options.TopK)
	freqPenalty := cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatalogModel.Options.FrequencyPenalty)
	presPenalty := cmp.Or(model.ModelCfg.PresencePenalty, model.CatalogModel.Options.PresencePenalty)
	return modelOptions, temp, topP, topK, freqPenalty, presPenalty, nil
}

func instructionPolicyForConstruction(construction providerregistry.Construction) fantasy.InstructionPolicy {
	switch construction {
	case providerregistry.ConstructionAnthropicMessages:
		return fantasy.InstructionPolicyAnthropic
	case providerregistry.ConstructionCodex:
		return fantasy.InstructionPolicyCodex
	default:
		return fantasy.InstructionPolicyGeneric
	}
}

const subagentPolicyInstructions = `<subagent_execution>
You are a subagent working under a main agent. Complete any delegated task directly and flexibly, stay within its scope, and return the result to the main agent.
Do not launch or continue other agents, start background tasks, or use codebase_search. Use the direct tools available to you and report blockers instead of leaving the delegated scope.
</subagent_execution>`

func applySubagentInstructions(instructions fantasy.Instructions, isSubAgent bool, roleInstructions string) fantasy.Instructions {
	if !isSubAgent {
		return instructions
	}
	sections := instructions.Sections()
	filtered := make([]fantasy.InstructionSection, 0, len(sections)+2)
	for _, section := range sections {
		if section.Stability == fantasy.InstructionStabilityStatic || section.Kind == fantasy.InstructionKindProviderContext {
			filtered = append(filtered, section)
		}
	}
	return fantasy.NewInstructions(filtered...).Append(
		fantasy.DynamicInstruction(fantasy.InstructionKindTooling, subagentPolicyInstructions),
		fantasy.DynamicInstruction(fantasy.InstructionKindAuxiliary, strings.TrimSpace(roleInstructions)),
	)
}

func (c *coordinator) currentSubagentPromptTemplate() (*prompt.Prompt, error) {
	c.skillsMu.RLock()
	promptTemplate := c.systemPromptTemplate
	activeSkills := slices.Clone(c.activeSkills)
	c.skillsMu.RUnlock()
	if promptTemplate != nil {
		return promptTemplate, nil
	}
	return coderPrompt(
		prompt.WithWorkingDir(c.cfg.WorkingDir()),
		prompt.WithSkills(activeSkills),
	)
}

func (c *coordinator) systemPromptBuilder(promptTemplate *prompt.Prompt, snapshot config.RuntimeSnapshot, isSubAgent bool, roleInstructions string) SystemPromptBuilder {
	return func(ctx context.Context, current session.Session, model Model) (fantasy.Instructions, error) {
		lifecycle, err := promptLifecycle(current)
		if err != nil {
			return fantasy.Instructions{}, err
		}
		instructions, err := promptTemplate.BuildLifecycleInstructionsWithSnapshot(ctx, model.ModelCfg.Provider, model.Model.Model(), c.cfg, snapshot, lifecycle)
		if err != nil {
			return fantasy.Instructions{}, err
		}
		return applySubagentInstructions(instructions, isSubAgent, roleInstructions), nil
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
	return c.buildAgentWithSnapshot(ctx, promptTemplate, agent, isSubAgent, c.cfg.RuntimeSnapshot())
}

func (c *coordinator) buildAgentWithSnapshot(ctx context.Context, promptTemplate *prompt.Prompt, agent config.Agent, isSubAgent bool, snapshot config.RuntimeSnapshot) (SessionAgent, error) {
	cfg := snapshot.Config()
	large, small, err := c.buildAgentModelsWithSnapshot(ctx, agent, isSubAgent, snapshot)
	if err != nil {
		return nil, err
	}
	if c.generationBoundary != nil {
		c.generationBoundary()
	}

	largeProviderCfg, _ := cfg.Providers.Get(large.ModelCfg.Provider)
	result := NewSessionAgent(SessionAgentOptions{
		LargeModel:           large,
		SmallModel:           small,
		SystemPromptPrefix:   largeProviderCfg.SystemPromptPrefix,
		SystemPrompt:         "",
		SystemPromptBuilder:  c.systemPromptBuilder(promptTemplate, snapshot, isSubAgent, agent.Instructions),
		RuntimeSnapshot:      snapshot,
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: cfg.Options.DisableAutoSummarize,
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
		instructions, err := promptTemplate.BuildInstructionsWithSnapshot(initCtx, large.ModelCfg.Provider, large.Model.Model(), c.cfg, snapshot)
		if err != nil {
			return err
		}
		result.SetInstructions(applySubagentInstructions(instructions, isSubAgent, agent.Instructions))
		return nil
	})

	c.readyWg.Go(func() error {
		palettes, err := c.buildToolsWithSnapshot(initCtx, agent, isSubAgent, snapshot)
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

func resolveSubagentTools(agent config.Agent, disabled []string) []string {
	allowed := slices.Clone(agent.AllowedTools)
	if agent.ID == config.AgentTask && !slices.Contains(disabled, tools.FetchToolName) && !slices.Contains(allowed, tools.FetchToolName) {
		allowed = append(allowed, tools.FetchToolName)
	}
	return slices.DeleteFunc(allowed, func(name string) bool {
		switch name {
		case AgentToolName,
			tools.AgenticFetchToolName,
			tools.CodebaseSearchToolName,
			tools.ImagegenToolName,
			tools.TaskContinueToolName,
			tools.TrafficCaptureToolName:
			return true
		default:
			return false
		}
	})
}

func (c *coordinator) buildTools(ctx context.Context, agent config.Agent, isSubAgent bool) (toolPalettes, error) {
	return c.buildToolsWithSnapshot(ctx, agent, isSubAgent, c.cfg.RuntimeSnapshot())
}

func (c *coordinator) buildToolsWithSnapshot(ctx context.Context, agent config.Agent, isSubAgent bool, runtimeSnapshot config.RuntimeSnapshot) (toolPalettes, error) {
	cfg := runtimeSnapshot.Config()
	var snapshot skills.Snapshot
	if c.skillsManager != nil {
		snapshot = c.skillsManager.Snapshot()
	} else {
		snapshot = discoverSkillSnapshotWithRuntime(c.cfg, runtimeSnapshot)
	}
	snapshot = providerSkillSnapshot(snapshot, cfg, selectedAgentProvider(cfg, agent))
	c.skillsMu.RLock()
	tracker := c.skillTracker
	c.skillsMu.RUnlock()
	if isSubAgent {
		tracker = tracker.CloneForActive(snapshot.ActiveSkills)
	}
	return c.buildToolsForSkills(ctx, agent, isSubAgent, snapshot, tracker, runtimeSnapshot)
}

func (c *coordinator) buildToolsForSkills(ctx context.Context, agent config.Agent, isSubAgent bool, snapshot skills.Snapshot, tracker *skills.Tracker, runtimeSnapshot config.RuntimeSnapshot) (toolPalettes, error) {
	cfg := runtimeSnapshot.Config()
	environment := runtimeSnapshot.Environment()
	var allTools []fantasy.AgentTool
	if !isSubAgent && slices.Contains(agent.AllowedTools, AgentToolName) {
		subagentPromptTemplate, err := coderPrompt(
			prompt.WithWorkingDir(c.cfg.WorkingDir()),
			prompt.WithSkills(snapshot.ActiveSkills),
		)
		if err != nil {
			return toolPalettes{}, err
		}
		agentTool, err := c.agentToolWithSnapshot(ctx, subagentPromptTemplate, runtimeSnapshot)
		if err != nil {
			return toolPalettes{}, err
		}
		allTools = append(allTools, agentTool)
	}

	if !isSubAgent && slices.Contains(agent.AllowedTools, tools.AgenticFetchToolName) {
		agenticFetchTool, err := c.agenticFetchTool(ctx, nil)
		if err != nil {
			return toolPalettes{}, err
		}
		allTools = append(allTools, agenticFetchTool)
	}

	logFile := filepath.Join(cfg.Options.DataDirectory, "logs", "crux.log")
	memoryService := automemory.NewService(c.cfg.WorkingDir())
	projectService := projects.NewService()

	// Build hook runner if PreToolUse hooks are configured.
	var hookRunner *hooks.Runner
	if preToolHooks := cfg.Hooks[hooks.EventPreToolUse]; len(preToolHooks) > 0 {
		hookRunner = hooks.NewRunner(preToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir(), environment)
	}

	allTools = append(
		allTools,
		tools.NewBashTool(c.backgroundShells, c.permissions, c.cfg.WorkingDir(), environment),
		tools.NewImagegenTool(c.backgroundImages, c.permissions, c.cfg.WorkingDir(), c.interactive),
		tools.NewJQTool(c.permissions, c.cfg.WorkingDir(), environment),
		tools.NewCruxInfoTool(c.cfg, c.lspManager, snapshot.AllSkills, snapshot.ActiveSkills, tracker),
		tools.NewCruxLogsTool(logFile),
		tools.NewTrafficLogsTool(),
		tools.NewTrafficLogDetailTool(),
		tools.NewTrafficLogSearchTool(),
		tools.NewTrafficCaptureTool(c.permissions, c.cfg.WorkingDir()),
		tools.NewGitInspectTool(c.cfg.WorkingDir(), environment),
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
		tools.NewSearchTool(c.permissions, c.cfg.WorkingDir(), cfg.Tools.Search),
		tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), cfg.Tools.Ls),
		tools.NewMemoryListTool(memoryService),
		tools.NewMemoryUpsertTool(memoryService),
		tools.NewMemoryRemoveTool(memoryService),
		tools.NewProjectCreateTool(projectService, c.cfg.WorkingDir()),
		tools.NewProjectStatusTool(projectService, c.cfg.WorkingDir()),
		tools.NewProjectUpdateTool(projectService, c.cfg.WorkingDir()),
		tools.NewProjectNotesTool(projectService, c.cfg.WorkingDir()),
		tools.NewProjectCompleteTool(projectService, c.cfg.WorkingDir()),
		tools.NewSkillListTool(snapshot.ActiveSkills, snapshot.ResolvedPaths, c.cfg.WorkingDir(), tracker),
		tools.NewSkillLoadTool(snapshot.ActiveSkills, snapshot.ResolvedPaths, c.cfg.WorkingDir(), tracker),
		tools.NewSourcegraphTool(nil),
		tools.NewTodosTool(c.sessions),
		tools.NewViewTool(c.lspManager, c.permissions, c.filetracker, tracker, c.cfg.WorkingDir(), snapshot.ResolvedPaths...),
		tools.NewWriteTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
	)
	if !isSubAgent && cfg.Tools.CodebaseSearch.IsEnabled() {
		allTools = append(allTools, tools.NewCodebaseSearchTool(c.cfg.WorkingDir(), cfg.Tools.CodebaseSearch, nil, c.requestCodebaseIndexReconcile))
	}
	if agent.Script != nil {
		allTools = append(allTools, tools.NewScriptTool(c.permissions, c.cfg.WorkingDir(), *agent.Script, environment))
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
	if len(cfg.LSP) > 0 || cfg.Options.AutoLSP == nil || *cfg.Options.AutoLSP {
		allTools = append(
			allTools,
			tools.NewDiagnosticsTool(c.lspManager),
			tools.NewReferencesTool(c.lspManager),
			tools.NewLSPRestartTool(c.lspManager),
			tools.NewSymbolsTool(c.lspManager),
			tools.NewDefinitionTool(c.lspManager),
			tools.NewCallHierarchyTool(c.lspManager),
			tools.NewRenameTool(c.lspManager, c.permissions, c.history, c.filetracker),
			tools.NewReplaceSymbolTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		)
	}

	if len(cfg.MCP) > 0 {
		allTools = append(
			allTools,
			tools.NewListMCPResourcesTool(c.cfg, c.permissions),
			tools.NewReadMCPResourceTool(c.cfg, c.permissions),
		)
	}

	var disabled []string
	if cfg.Options != nil {
		disabled = cfg.Options.DisabledTools
	}
	if agent.DefinitionPath != "" {
		available := make([]string, 0, len(allTools))
		for _, tool := range allTools {
			available = append(available, tool.Info().Name)
		}
		resolvedTools, err := resolveCustomAgentTools(agent, available, disabled)
		if err != nil {
			return toolPalettes{}, err
		}
		agent.AllowedTools = resolvedTools
	}
	if isSubAgent {
		agent.AllowedTools = resolveSubagentTools(agent, disabled)
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
	return c.buildAgentModelsWithSnapshot(ctx, agent, isSubAgent, c.cfg.RuntimeSnapshot())
}

func (c *coordinator) buildAgentModelsWithSnapshot(ctx context.Context, agent config.Agent, isSubAgent bool, snapshot config.RuntimeSnapshot) (Model, Model, error) {
	cfg := snapshot.Config()
	var primaryModelCfg config.SelectedModel
	if agent.PrimaryModelOverride != nil {
		primaryModelCfg = *agent.PrimaryModelOverride
		if !cfg.IsModelAvailable(primaryModelCfg.Provider, primaryModelCfg.Model) {
			return Model{}, Model{}, fmt.Errorf("primary model %q for provider %q is not available", primaryModelCfg.Model, primaryModelCfg.Provider)
		}
	} else {
		var ok bool
		primaryModelCfg, ok = cfg.Models[agent.Model]
		if !ok {
			return Model{}, Model{}, errLargeModelNotSelected
		}
	}
	smallModelCfg, ok := cfg.Models[config.SelectedModelTypeSmall]
	if !ok {
		return Model{}, Model{}, errSmallModelNotSelected
	}

	primaryProviderCfg, ok := cfg.Providers.Get(primaryModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errLargeModelProviderNotConfigured
	}
	primaryProvider, err := c.buildProvider(snapshot, primaryProviderCfg, primaryModelCfg, isSubAgent)
	if err != nil {
		return Model{}, Model{}, err
	}

	smallProviderCfg, ok := cfg.Providers.Get(smallModelCfg.Provider)
	if !ok {
		return Model{}, Model{}, errSmallModelProviderNotConfigured
	}
	smallProvider, err := c.buildProvider(snapshot, smallProviderCfg, smallModelCfg, true)
	if err != nil {
		return Model{}, Model{}, err
	}

	var primaryCatalogModel *catalog.Model
	var smallCatalogModel *catalog.Model
	for _, model := range primaryProviderCfg.Models {
		if model.ID == primaryModelCfg.Model {
			primaryCatalogModel = &model
			break
		}
	}
	for _, model := range smallProviderCfg.Models {
		if model.ID == smallModelCfg.Model {
			smallCatalogModel = &model
			break
		}
	}
	if primaryCatalogModel == nil {
		return Model{}, Model{}, errLargeModelNotFound
	}
	if smallCatalogModel == nil {
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
	primaryRegistration, primaryRegistered := snapshot.ProviderBehaviorRegistration(primaryModelCfg.Provider, primaryProviderCfg)
	primaryCompaction, primaryCompactionRetry, err := manifestCompaction(primaryRegistration, primaryRegistered)
	if err != nil {
		return Model{}, Model{}, fmt.Errorf("provider %s compaction: %w", primaryModelCfg.Provider, err)
	}
	var primaryCompactor RemoteCompactor
	if primaryCompaction != nil && primaryCompaction.Mode == "remote-operation" {
		var ok bool
		primaryCompactor, ok = primaryLanguageModel.(RemoteCompactor)
		if !ok {
			return Model{}, Model{}, fmt.Errorf("provider %s remote compaction executor is unavailable", primaryModelCfg.Provider)
		}
		primaryCompactor = mapRemoteCompactorErrors(primaryCompactor, primaryRegistration)
	}
	if primaryRegistered {
		primaryLanguageModel = mapLanguageModelErrors(primaryLanguageModel, primaryRegistration)
		if primaryRegistration.Construction == providerregistry.ConstructionOpenAIResponses {
			continuationProvider, ok := primaryProvider.(nativeResponsesContinuationProvider)
			if !ok || continuationProvider.continuationOwner() == "" {
				return Model{}, Model{}, fmt.Errorf("provider %s native Responses continuation owner is unavailable", primaryModelCfg.Provider)
			}
			primaryLanguageModel = openairesponsestransport.NewLifecycleModelWithErrorMappingsAndStore(primaryLanguageModel, primaryRegistration.Operation.Retry, primaryRegistration.Operation.Continuation, primaryRegistration.Errors, c.responsesContinuations, continuationProvider.continuationOwner(), primaryRegistration.Operation.ToolCodec)
		}
	}
	primaryImagePolicy, hasPrimaryImagePolicy := imageattachment.PolicyFromDeclaration(primaryRegistration.Images)

	smallRegistration, smallRegistered := snapshot.ProviderBehaviorRegistration(smallModelCfg.Provider, smallProviderCfg)
	smallCompaction, smallCompactionRetry, err := manifestCompaction(smallRegistration, smallRegistered)
	if err != nil {
		return Model{}, Model{}, fmt.Errorf("provider %s compaction: %w", smallModelCfg.Provider, err)
	}
	var smallCompactor RemoteCompactor
	if smallCompaction != nil && smallCompaction.Mode == "remote-operation" {
		var ok bool
		smallCompactor, ok = smallLanguageModel.(RemoteCompactor)
		if !ok {
			return Model{}, Model{}, fmt.Errorf("provider %s remote compaction executor is unavailable", smallModelCfg.Provider)
		}
		smallCompactor = mapRemoteCompactorErrors(smallCompactor, smallRegistration)
	}
	if smallRegistered {
		smallLanguageModel = mapLanguageModelErrors(smallLanguageModel, smallRegistration)
		if smallRegistration.Construction == providerregistry.ConstructionOpenAIResponses {
			continuationProvider, ok := smallProvider.(nativeResponsesContinuationProvider)
			if !ok || continuationProvider.continuationOwner() == "" {
				return Model{}, Model{}, fmt.Errorf("provider %s native Responses continuation owner is unavailable", smallModelCfg.Provider)
			}
			smallLanguageModel = openairesponsestransport.NewLifecycleModelWithErrorMappingsAndStore(smallLanguageModel, smallRegistration.Operation.Retry, smallRegistration.Operation.Continuation, smallRegistration.Errors, c.responsesContinuations, continuationProvider.continuationOwner(), smallRegistration.Operation.ToolCodec)
		}
	}
	smallImagePolicy, hasSmallImagePolicy := imageattachment.PolicyFromDeclaration(smallRegistration.Images)

	primary := Model{
		Model:              primaryLanguageModel,
		CatalogModel:       *primaryCatalogModel,
		ModelCfg:           primaryModelCfg,
		FlatRate:           primaryProviderCfg.FlatRate,
		SystemPromptPrefix: primaryProviderCfg.SystemPromptPrefix,
		InstructionPolicy:  instructionPolicyForConstruction(primaryRegistration.Construction),
		Compaction:         primaryCompaction,
		Compactor:          primaryCompactor,
		CompactionRetry:    primaryCompactionRetry,
		Retry:              manifestOperationRetry(primaryRegistration, primaryRegistered),
		Metadata:           slices.Clone(primaryRegistration.Metadata),
		OnAuthRefresh:      c.makeAuthRefreshCallback(snapshot, primaryProviderCfg),
	}
	if !primaryRegistered {
		primary.InstructionPolicy = fantasy.InstructionPolicyGeneric
	}
	if hasPrimaryImagePolicy {
		primary.ImagePolicy = &primaryImagePolicy
	}
	primaryProviderOptions, err := getProviderOptions(primary, primaryProviderCfg, primaryRegistration)
	if err != nil {
		return Model{}, Model{}, fmt.Errorf("provider %s options: %w", primaryModelCfg.Provider, err)
	}
	primary.ProviderOptions = c.oauthReasoningOptionsForRegistration(
		primaryModelCfg.Provider,
		primaryModelCfg.Model,
		primaryRegistration,
		primaryRegistered,
		primaryProviderOptions,
	)
	small := Model{
		Model:              smallLanguageModel,
		CatalogModel:       *smallCatalogModel,
		ModelCfg:           smallModelCfg,
		FlatRate:           smallProviderCfg.FlatRate,
		SystemPromptPrefix: smallProviderCfg.SystemPromptPrefix,
		InstructionPolicy:  instructionPolicyForConstruction(smallRegistration.Construction),
		Compaction:         smallCompaction,
		Compactor:          smallCompactor,
		CompactionRetry:    smallCompactionRetry,
		Retry:              manifestOperationRetry(smallRegistration, smallRegistered),
		Metadata:           slices.Clone(smallRegistration.Metadata),
		OnAuthRefresh:      c.makeAuthRefreshCallback(snapshot, smallProviderCfg),
	}
	if !smallRegistered {
		small.InstructionPolicy = fantasy.InstructionPolicyGeneric
	}
	if hasSmallImagePolicy {
		small.ImagePolicy = &smallImagePolicy
	}
	smallProviderOptions, err := getProviderOptions(small, smallProviderCfg, smallRegistration)
	if err != nil {
		return Model{}, Model{}, fmt.Errorf("provider %s options: %w", smallModelCfg.Provider, err)
	}
	small.ProviderOptions = c.oauthReasoningOptionsForRegistration(
		smallModelCfg.Provider,
		smallModelCfg.Model,
		smallRegistration,
		smallRegistered,
		smallProviderOptions,
	)
	return primary, small, nil
}

func manifestOperationRetry(registration providerregistry.Registration, registered bool) *manifest.RetryPolicy {
	if !registered || registration.Manifest == nil || registration.Operation == nil {
		return nil
	}
	policy := registration.Operation.Retry
	return &policy
}

func manifestCompaction(registration providerregistry.Registration, registered bool) (*manifest.CompactionPolicy, *manifest.RetryPolicy, error) {
	if !registered || registration.Operation == nil || registration.Operation.Compaction == nil {
		return nil, nil, nil
	}
	policy := *registration.Operation.Compaction
	switch policy.Mode {
	case "none", "local-summary":
		return &policy, nil, nil
	case "remote-operation":
		operation, ok := registration.Operations[policy.Operation]
		if !ok || operation == nil {
			return nil, nil, fmt.Errorf("remote operation %q is unavailable", policy.Operation)
		}
		retry := operation.Retry
		retry.Statuses = slices.Clone(retry.Statuses)
		retry.Codes = slices.Clone(retry.Codes)
		return &policy, &retry, nil
	default:
		return nil, nil, fmt.Errorf("mode %q is unsupported", policy.Mode)
	}
}

func (c *coordinator) buildAnthropicProvider(debug bool, baseURL, apiKey string, headers map[string]string, validate providertransport.OwnerValidator) (fantasy.Provider, error) {
	opts := []anthropic.Option{anthropic.WithAPIKey(apiKey)}
	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}
	httpClient := http.DefaultClient
	if debug {
		httpClient = log.NewHTTPClient()
	}
	opts = append(opts, anthropic.WithHTTPClient(providertransport.ClientWithOwnerValidator(httpClient, validate)))
	return anthropic.New(opts...)
}

// buildManifestAnthropicProvider is the required construction path for an
// Anthropic Messages registration with manifest wire policy. The custom client
// resolves the declared client version and user agent, enforces endpoint and
// protected-header policy, rewrites request metadata and tools, and remaps
// streamed tool names. Replacing it with the generic Anthropic client silently
// removes those plugin contracts even though provider construction still
// appears successful.
func (c *coordinator) buildManifestAnthropicProvider(debug bool, registration providerregistry.Registration, baseURL, apiKey string, headers map[string]string, values providertransport.TemplateValues, validate providertransport.OwnerValidator) (fantasy.Provider, error) {
	baseURL, err := anthropictransport.EffectiveBaseURL(registration.Operation, baseURL)
	if err != nil {
		return nil, err
	}
	operation, err := registration.Operation.BindTemplates(values)
	if err != nil {
		return nil, err
	}
	operation.Endpoint.BaseURL = baseURL
	httpClient, err := anthropictransport.NewClient(operation, debug, validate)
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

func nativeResponsesContinuationOwner(snapshot config.RuntimeSnapshot, registration providerregistry.Registration, providerID, baseURL, apiKey string) string {
	owner := registration.Owner()
	account := "anonymous"
	if apiKey != "" {
		account = "credential:" + hashContinuationIdentity(apiKey)
	}
	if owner.AccountNamespace != "" {
		entry, ok := snapshot.EphemeralAccount(owner)
		if !ok {
			entry, _ = accounts.Active(context.Background(), owner.AccountNamespace)
		}
		if entry != nil && entry.ID != "" && entry.AccessToken == apiKey {
			account = "account:" + entry.ID
		}
	}
	identity, err := json.Marshal(struct {
		Endpoint   string                             `json:"endpoint"`
		Owner      providerregistry.RegistrationOwner `json:"owner"`
		ProviderID string                             `json:"provider_id"`
		Account    string                             `json:"account"`
	}{
		Endpoint:   baseURL,
		Owner:      owner,
		ProviderID: providerID,
		Account:    account,
	})
	if err != nil {
		return ""
	}
	return hashContinuationIdentity(string(identity))
}

func hashContinuationIdentity(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest)
}

func (c *coordinator) buildOpenaiProvider(debug bool, options *config.Options, registration providerregistry.Registration, baseURL, apiKey string, headers map[string]string, values providertransport.TemplateValues, validate providertransport.OwnerValidator) (fantasy.Provider, error) {
	operation := registration.Operation.Clone()
	operation.Endpoint.BaseURL = baseURL
	operation.Retry.MaxAttempts = 1
	baseClient := http.DefaultClient
	if debug {
		baseClient = log.NewHTTPClient()
	}
	runtimeControls, err := runtimeControlValues(registration.RuntimeControls, options)
	if err != nil {
		return nil, fmt.Errorf("provider %s runtime controls: %w", registration.ProviderID, err)
	}
	policyTransport := &providertransport.PolicyTransport{
		Base:            providertransport.OwnerValidatingTransport(baseClient.Transport, validate),
		Operation:       operation,
		Values:          values,
		Headers:         headers,
		RuntimeControls: runtimeControls,
	}
	httpClient := *baseClient
	httpClient.Transport = policyTransport
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
		openai.WithResponsesAPIFunc(func(string) bool { return true }),
		openai.WithHTTPClient(&httpClient),
		openai.WithBaseURL(baseURL),
	}
	return openai.New(opts...)
}

func (c *coordinator) buildOpenaiCompatProvider(debug bool, baseURL, apiKey string, headers map[string]string, extraBody map[string]any, construction providerregistry.Construction, isSubAgent bool, validate providertransport.OwnerValidator) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	switch construction {
	case providerregistry.ConstructionCopilot:
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(func(modelID string) bool {
				return copilotResponsesModels[modelID]
			}),
		)
		httpClient = copilotinference.NewClient(isSubAgent, debug)
	}
	if httpClient == nil && debug {
		httpClient = log.NewHTTPClient()
	}
	httpClient = providertransport.ClientWithOwnerValidator(httpClient, validate)
	opts = append(opts, openaicompat.WithHTTPClient(httpClient))

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
func (c *coordinator) buildCodexProvider(registration providerregistry.Registration, baseURL, apiKey string, headers map[string]string, validate providertransport.OwnerValidator) (fantasy.Provider, error) {
	accountID := func() string {
		if registration.AccountNamespace == "" {
			return ""
		}
		entry, ok := c.cfg.EphemeralAccount(registration.Owner())
		if !ok {
			var err error
			entry, err = accounts.Active(context.Background(), registration.AccountNamespace)
			if err != nil {
				return ""
			}
		}
		if entry == nil || entry.AccessToken != apiKey {
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
	var compactionOperation *providertransport.Operation
	if registration.Operation != nil {
		if policy := registration.Operation.Compaction; policy != nil && policy.Mode == "remote-operation" {
			var ok bool
			compactionOperation, ok = registration.Operations[policy.Operation]
			if !ok || compactionOperation == nil {
				return nil, fmt.Errorf("Codex remote compaction operation %q is unavailable", policy.Operation)
			}
		}
	}
	return codex.NewProvider(baseURL, func() string { return apiKey }, accountID, headers, c.codexSessions, registration.Operation, compactionOperation, registration.Images, validate)
}

// buildGeminiAntigravityProvider creates the Antigravity provider: a native
// Antigravity-dialect provider pointed at the Cloud Code v1internal endpoint,
// authenticating with an OAuth Bearer token.
func (c *coordinator) buildGeminiAntigravityProvider(registration providerregistry.Registration, baseURL, apiKey string, headers map[string]string, validate providertransport.OwnerValidator) (fantasy.Provider, error) {
	return gemini.NewProvider(baseURL, func() string { return apiKey }, headers, registration.Operation, validate)
}

func (c *coordinator) buildDeclarativeProvider(debug bool, options *config.Options, registration providerregistry.Registration, baseURL string, headers map[string]string, values providertransport.TemplateValues, validate providertransport.OwnerValidator) (fantasy.Provider, error) {
	operation := registration.Operation.Clone()
	operation.Endpoint.BaseURL = baseURL
	baseClient := http.DefaultClient
	if debug {
		baseClient = log.NewHTTPClient()
	}
	httpClient := *baseClient
	httpClient.Transport = providertransport.OwnerValidatingTransport(
		providertransport.TransportWithStreamIdleTimeout(
			providertransport.TransportWithConnectTimeout(baseClient.Transport, operation.ConnectTimeout),
			operation.StreamIdleTimeout,
		),
		validate,
	)
	runtimeControls, err := runtimeControlValues(registration.RuntimeControls, options)
	if err != nil {
		return nil, fmt.Errorf("provider %s runtime controls: %w", registration.ProviderID, err)
	}
	usage := registration.Usage
	if usage != nil && usage.Source == "operation" {
		usage = nil
	}
	metadataSchemas, err := manifest.CompileMetadataContracts(registration.Metadata)
	if err != nil {
		return nil, fmt.Errorf("provider %s metadata contracts: %w", registration.ProviderID, err)
	}
	return &declarativetransport.Provider{
		ID:              registration.ProviderID,
		Operation:       operation,
		Usage:           usage,
		Errors:          registration.Errors,
		Metadata:        registration.Metadata,
		MetadataSchemas: metadataSchemas,
		Headers:         headers,
		Values:          values,
		HTTPClient:      &httpClient,
		RuntimeControl:  runtimeControls,
	}, nil
}

func manifestTemplateValues(registration providerregistry.Registration, providerCfg config.ProviderConfig, apiKey string) (providertransport.TemplateValues, error) {
	values := providertransport.TemplateValues{
		Config:      maps.Clone(providerCfg.Configuration),
		Credentials: map[string]string{},
		Context:     map[string]string{"client.user_agent": "Crux", "oauth.access_token": apiKey},
	}
	if registration.Manifest == nil {
		return values, nil
	}
	for _, credential := range registration.Manifest.Capabilities.Credentials {
		if credential.ConfigProperty != "" {
			configured, ok := providerCfg.Configuration[credential.ConfigProperty]
			if !ok {
				return providertransport.TemplateValues{}, fmt.Errorf("credential %q requires configuration property %q", credential.ID, credential.ConfigProperty)
			}
			text, ok := configured.(string)
			if !ok || text == "" {
				return providertransport.TemplateValues{}, fmt.Errorf("credential %q configuration property %q must be a non-empty string", credential.ID, credential.ConfigProperty)
			}
			values.Credentials[credential.ID] = text
			continue
		}
		switch credential.Kind {
		case "oauth2", "bearer", "api-key":
			if apiKey == "" {
				return providertransport.TemplateValues{}, fmt.Errorf("credential %q is unavailable", credential.ID)
			}
			values.Credentials[credential.ID] = apiKey
		case "none":
		default:
			return providertransport.TemplateValues{}, fmt.Errorf("credential %q uses unsupported kind %q", credential.ID, credential.Kind)
		}
	}
	return values, nil
}

func runtimeControlValues(controls []manifest.RuntimeControl, options *config.Options) (map[string]any, error) {
	result := map[string]any{}
	for _, control := range controls {
		var value any
		if options != nil {
			switch {
			case providerregistry.IsResponseVerbosityControl(control) && options.ResponseVerbosity != "":
				value = options.ResponseVerbosity
			case providerregistry.IsAnalysisEffortControl(control) && options.AnalysisEffort != "":
				value = options.AnalysisEffort
			}
		}
		if value == nil {
			value = control.Default
		}
		if value == nil || control.RequestPath == "" {
			continue
		}
		if err := validateRuntimeControlValue(control, value); err != nil {
			return nil, err
		}
		result[control.RequestPath] = value
	}
	return result, nil
}

func validateRuntimeControlValue(control manifest.RuntimeControl, value any) error {
	switch control.Type {
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("control %q requires a boolean", control.ID)
		}
	case "integer":
		switch number := value.(type) {
		case int, int32, int64:
		case float64:
			if number != float64(int64(number)) {
				return fmt.Errorf("control %q requires an integer", control.ID)
			}
		default:
			return fmt.Errorf("control %q requires an integer", control.ID)
		}
	case "number":
		switch value.(type) {
		case int, int32, int64, float32, float64:
		default:
			return fmt.Errorf("control %q requires a number", control.ID)
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("control %q requires a string", control.ID)
		}
	case "enum":
		text, ok := value.(string)
		if !ok || !slices.Contains(control.Values, text) {
			return fmt.Errorf("control %q has invalid explicit value %q", control.ID, text)
		}
	default:
		return fmt.Errorf("control %q uses unsupported type %q", control.ID, control.Type)
	}
	return nil
}

// buildProvider is the runtime ownership gate for configured providers. A
// durable plugin or preset marker requires its exact active owner; absence must
// fail clearly and must never fall through to another registration or a generic
// provider constructor. Registered construction, operation validation, endpoint
// policy, and custom transports are one contract. Do not bypass any part based
// only on a familiar provider ID or protocol name.
func openAICompatExtraBody(providerCfg config.ProviderConfig) map[string]any {
	extraBody := maps.Clone(providerCfg.ExtraBody)
	if legacyProviderBehaviorID(providerCfg.ID, providerCfg) != string(catalog.ProviderZAI) {
		return extraBody
	}
	if extraBody == nil {
		extraBody = make(map[string]any)
	}
	extraBody["tool_stream"] = true
	return extraBody
}

func (c *coordinator) buildProvider(snapshot config.RuntimeSnapshot, providerCfg config.ProviderConfig, selectedModel config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	providerCfg, registration, registered, err := snapshot.ProviderForConstruction(selectedModel.Provider, providerCfg)
	if err != nil {
		return nil, err
	}
	owner, active := snapshot.ProviderOwnerFor(providerCfg.ID, providerCfg)
	if !active {
		return nil, fmt.Errorf("provider %s exact owner is unavailable before construction", providerCfg.ID)
	}
	validateOwner := func() error {
		return c.cfg.ValidateActiveProviderOwner(owner)
	}
	cfg := snapshot.Config()
	debug := cfg != nil && cfg.Options != nil && cfg.Options.Debug
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	if !registered && providerCfg.OAuthToken != nil && providerCfg.Type != catalog.TypeOpenAICompat {
		return nil, fmt.Errorf("OAuth provider %s is unavailable because its registered integration is not active; install, trust, enable, or select the required provider plugin", providerCfg.ID)
	}

	apiKey, err := snapshot.Resolve(providerCfg.APIKey)
	if err != nil {
		return nil, fmt.Errorf("resolve provider %s credential: %w", providerCfg.ID, err)
	}
	baseURL, err := snapshot.Resolve(providerCfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("resolve provider %s endpoint: %w", providerCfg.ID, err)
	}
	values := providertransport.TemplateValues{}
	if registered && registration.Operation != nil {
		baseURL, err = registration.Operation.ResolveEndpoint(baseURL)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
		}
		values, err = manifestTemplateValues(registration, providerCfg, apiKey)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
		}
		values.Context["client.version"] = fantasy.Version
		values.Context["session.id"] = uuid.NewString()
		if registration.Construction != providerregistry.ConstructionAnthropicMessages {
			headers, err = registration.Operation.ApplyHeadersWithValues(headers, values)
			if err != nil {
				return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
			}
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
			return c.buildGeminiAntigravityProvider(registration, baseURL, apiKey, headers, validateOwner)
		case providerregistry.ConstructionCodex:
			if registration.Operation != nil {
				if err := registration.Operation.ValidateSelection("openai-responses", "websocket-json"); err != nil {
					return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
				}
			}
			return c.buildCodexProvider(registration, baseURL, apiKey, headers, validateOwner)
		case providerregistry.ConstructionCopilot:
			return c.buildOpenaiCompatProvider(debug, baseURL, apiKey, headers, providerCfg.ExtraBody, registration.Construction, isSubAgent, validateOwner)
		case providerregistry.ConstructionAnthropicMessages:
			if registration.Operation == nil {
				return nil, fmt.Errorf("provider %s has no operation contract", providerCfg.ID)
			}
			if err := registration.Operation.ValidateSelection(string(providerregistry.ConstructionAnthropicMessages), "sse"); err != nil {
				return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
			}
			if registration.Operation.Anthropic != nil {
				return c.buildManifestAnthropicProvider(debug, registration, baseURL, apiKey, headers, values, validateOwner)
			}
			return c.buildAnthropicProvider(debug, baseURL, apiKey, headers, validateOwner)
		case providerregistry.ConstructionOpenAIResponses:
			if registration.Operation == nil {
				return nil, fmt.Errorf("provider %s has no operation contract", providerCfg.ID)
			}
			if err := registration.Operation.ValidateSelection(string(providerregistry.ConstructionOpenAIResponses), "sse"); err != nil {
				return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
			}
			provider, err := c.buildOpenaiProvider(debug, cfg.Options, registration, baseURL, apiKey, headers, values, validateOwner)
			if err != nil {
				return nil, err
			}
			continuationOwner := nativeResponsesContinuationOwner(snapshot, registration, providerCfg.ID, baseURL, apiKey)
			if continuationOwner == "" {
				return nil, fmt.Errorf("provider %s native Responses continuation identity is unavailable", providerCfg.ID)
			}
			return &nativeResponsesProvider{Provider: provider, owner: continuationOwner}, nil
		case providerregistry.ConstructionGeminiContent, providerregistry.ConstructionGeminiInteraction, providerregistry.ConstructionGenericJSON:
			if registration.Operation == nil {
				return nil, fmt.Errorf("provider %s has no operation contract", providerCfg.ID)
			}
			if err := registration.Operation.ValidateSelection(string(registration.Construction), "http-json", "sse"); err != nil {
				return nil, fmt.Errorf("provider %s: %w", providerCfg.ID, err)
			}
			return c.buildDeclarativeProvider(debug, cfg.Options, registration, baseURL, headers, values, validateOwner)
		default:
			return nil, fmt.Errorf("provider %s uses unsupported construction %q", providerCfg.ID, registration.Construction)
		}
	}

	switch providerCfg.Owner.Type {
	case config.ProviderOwnerCustom, config.ProviderOwnerPreset:
		if providerCfg.Type == openaicompat.Name {
			return c.buildOpenaiCompatProvider(debug, baseURL, apiKey, headers, openAICompatExtraBody(providerCfg), "", isSubAgent, validateOwner)
		}
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			return c.buildOpenaiCompatProvider(debug, baseURL, apiKey, headers, providerCfg.ExtraBody, "", isSubAgent, validateOwner)
		}
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	default:
		return nil, fmt.Errorf("provider %s owner type %q has no authorized construction", providerCfg.ID, providerCfg.Owner.Type)
	}
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

func (c *coordinator) ResetSession(sessionID string) {
	conversationID := session.HashID(sessionID)
	c.responsesContinuations.ResetConversation(conversationID)
	if c.currentAgent == nil {
		return
	}
	runtime := c.currentAgent.Runtime()
	for _, model := range []fantasy.LanguageModel{runtime.LargeModel.Model, runtime.SmallModel.Model} {
		if resetter, ok := model.(interface{ ResetConversationChain(string) }); ok {
			resetter.ResetConversationChain(conversationID)
		}
	}
}

func (c *coordinator) CancelAll() {
	c.currentAgent.CancelAll()
}

func (c *coordinator) CloseContext(ctx context.Context) {
	c.stopCodebaseIndexLifecycle(ctx)
	if c.backgroundAgents != nil {
		c.backgroundAgents.StopAll(ctx)
	}
	if c.backgroundImages != nil {
		c.backgroundImages.StopAll(ctx)
	}
	c.memoryWorker.Close()
	if c.codexSessions != nil {
		c.codexSessions.Close()
	}
	c.responsesContinuations.Close()
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

func (c *coordinator) instructionSnapshot(ctx context.Context) (InstructionSnapshot, error) {
	c.updateMu.Lock()
	defer c.updateMu.Unlock()

	runtime := c.currentAgent.Runtime()
	model := runtime.LargeModel
	if model.Model == nil {
		return InstructionSnapshot{}, errCoderAgentNotConfigured
	}
	catalogSnapshot := discoverSkillSnapshotWithRuntime(c.cfg, runtime.Snapshot)
	snapshot := providerSkillSnapshot(catalogSnapshot, runtime.Snapshot.Config(), model.ModelCfg.Provider)
	coder, err := coderPrompt(
		prompt.WithWorkingDir(c.cfg.WorkingDir()),
		prompt.WithSkills(snapshot.ActiveSkills),
	)
	if err != nil {
		return InstructionSnapshot{}, err
	}
	instructions, err := coder.BuildInstructionsWithSnapshot(ctx, model.ModelCfg.Provider, model.Model.Model(), c.cfg, runtime.Snapshot)
	if err != nil {
		return InstructionSnapshot{}, err
	}
	providerCfg, _ := runtime.Snapshot.Config().Providers.Get(model.ModelCfg.Provider)
	instructions = appendRuntimeInstructions(
		instructions,
		providerCfg.SystemPromptPrefix,
		connectedMCPInstructions(),
		"",
		"",
		"",
	)
	snapshotSections := instructionSnapshotSections(instructions, model.InstructionPolicy)
	return InstructionSnapshot{
		ProviderID: model.ModelCfg.Provider,
		ModelID:    model.Model.Model(),
		Policy:     model.InstructionPolicy,
		Sections:   snapshotSections,
	}, nil
}

func instructionSnapshotSections(instructions fantasy.Instructions, policy fantasy.InstructionPolicy) []InstructionSnapshotSection {
	message := instructions.Message(policy)
	sections := instructions.Sections()
	result := make([]InstructionSnapshotSection, len(sections))
	for index, section := range sections {
		cacheBoundary := false
		if index < len(message.Content) {
			if options := fantasy.InstructionPartOptionsFrom(message.Content[index].Options()); options != nil {
				cacheBoundary = options.CacheBoundary
			}
		}
		result[index] = InstructionSnapshotSection{
			Kind:          section.Kind,
			Stability:     section.Stability,
			Text:          section.Text,
			CacheBoundary: cacheBoundary,
		}
	}
	return result
}

func (c *coordinator) UpdateModels(ctx context.Context) error {
	return c.UpdateModelsForState(ctx, c.cfg.RuntimeSnapshot().AgentModelState())
}

func (c *coordinator) UpdateModelsForState(ctx context.Context, expected config.AgentModelState) error {
	if c.generationBoundary != nil {
		c.generationBoundary()
	}
	return c.cfg.WithRuntimeSnapshot(func(runtimeSnapshot config.RuntimeSnapshot) error {
		if err := runtimeSnapshot.ValidateAgentModelState(expected); err != nil {
			return err
		}
		candidate, err := c.prepareRuntimeGeneration(ctx, runtimeSnapshot)
		if err != nil {
			return err
		}
		defer candidate.Abort()
		candidate.Commit()
		return nil
	})
}

func (c *coordinator) prepareRuntimeGeneration(ctx context.Context, runtimeSnapshot config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
	c.updateMu.Lock()
	release := true
	defer func() {
		if release {
			c.updateMu.Unlock()
		}
	}()

	cfg := runtimeSnapshot.Config()
	agentCfg, ok := cfg.Agents[config.AgentCoder]
	if !ok {
		return config.RuntimeGenerationCandidate{}, errCoderAgentNotConfigured
	}

	large, small, err := c.buildAgentModelsWithSnapshot(ctx, agentCfg, false, runtimeSnapshot)
	if err != nil {
		return config.RuntimeGenerationCandidate{}, err
	}

	catalogSnapshot := discoverSkillSnapshotWithRuntime(c.cfg, runtimeSnapshot)
	snapshot := providerSkillSnapshot(catalogSnapshot, cfg, large.ModelCfg.Provider)
	c.skillsMu.RLock()
	previousTracker := c.skillTracker
	c.skillsMu.RUnlock()
	tracker := previousTracker.CloneForActive(snapshot.ActiveSkills)
	coder, err := coderPrompt(
		prompt.WithWorkingDir(c.cfg.WorkingDir()),
		prompt.WithSkills(snapshot.ActiveSkills),
	)
	if err != nil {
		return config.RuntimeGenerationCandidate{}, err
	}
	instructions, err := coder.BuildInstructionsWithSnapshot(ctx, large.ModelCfg.Provider, large.Model.Model(), c.cfg, runtimeSnapshot)
	if err != nil {
		return config.RuntimeGenerationCandidate{}, err
	}

	palettes, err := c.buildToolsForSkills(ctx, agentCfg, false, snapshot, tracker, runtimeSnapshot)
	if err != nil {
		return config.RuntimeGenerationCandidate{}, err
	}

	installed := InstalledRuntime{
		LargeModel:           large,
		SmallModel:           small,
		Instructions:         instructions,
		Tools:                palettes.normal,
		PlanModeTools:        palettes.planMode,
		DisableAutoSummarize: cfg.Options.DisableAutoSummarize,
		SystemPromptBuilder:  c.systemPromptBuilder(coder, runtimeSnapshot, false, agentCfg.Instructions),
		Snapshot:             runtimeSnapshot,
	}
	var once sync.Once
	finish := func(commit bool) {
		once.Do(func() {
			defer c.updateMu.Unlock()
			if !commit {
				return
			}
			if c.skillsManager != nil {
				catalogSnapshot = c.skillsManager.ReplaceSnapshot(catalogSnapshot)
			}
			c.currentAgent.SetRuntime(installed)
			c.skillsMu.Lock()
			c.allSkills = catalogSnapshot.AllSkills
			c.activeSkills = snapshot.ActiveSkills
			c.skillTracker = tracker
			c.systemPromptTemplate = coder
			c.skillsMu.Unlock()
			c.scheduleCodebaseIndexReconcile()
		})
	}
	release = false
	return config.RuntimeGenerationCandidate{
		Commit: func() { finish(true) },
		Abort:  func() { finish(false) },
	}, nil
}

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent.QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []QueuedPrompt {
	return c.currentAgent.QueuedPromptsList(sessionID)
}

func (c *coordinator) Summarize(ctx context.Context, sessionID string) error {
	runtime := c.currentAgent.Runtime()
	model := runtime.LargeModel
	cfg := runtime.Snapshot.Config()
	providerCfg, ok := cfg.Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return errModelProviderNotConfigured
	}
	providerCfg.ID = model.ModelCfg.Provider

	if err := c.refreshTokenIfExpired(ctx, runtime.Snapshot, providerCfg); err != nil {
		slog.Error("Failed to refresh OAuth2 token before summarize. Proceeding with existing token.", "provider", providerCfg.ID)
	}

	// Auth failures during summarize flow through fantasy's OnAuthRefresh,
	// the same path used by regular turns.
	registration, registered := runtime.Snapshot.ProviderBehaviorRegistration(model.ModelCfg.Provider, providerCfg)
	providerOptions, err := getProviderOptions(model, providerCfg, registration)
	if err != nil {
		return fmt.Errorf("provider %s options: %w", model.ModelCfg.Provider, err)
	}
	providerOptions = c.oauthReasoningOptionsForRegistration(model.ModelCfg.Provider, model.ModelCfg.Model, registration, registered, providerOptions)
	err = c.currentAgent.SummarizeWithRuntime(ctx, sessionID, providerOptions, c.makeAuthRefreshCallback(runtime.Snapshot, providerCfg), runtime)
	if err != nil {
		c.disableOAuthReasoningForRegistration(model.ModelCfg.Provider, model.ModelCfg.Model, registration, registered, err.Error())
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
func (c *coordinator) refreshTokenIfExpired(ctx context.Context, snapshot config.RuntimeSnapshot, providerCfg config.ProviderConfig) error {
	if providerCfg.OAuthToken == nil || !providerCfg.OAuthToken.IsExpired() {
		return nil
	}
	owner, ok := snapshot.ProviderOwnerFor(providerCfg.ID, providerCfg)
	if !ok {
		return fmt.Errorf("registration owner for provider %s is not active in the admitted runtime", providerCfg.ID)
	}
	slog.Debug("Token needs to be refreshed", "provider", providerCfg.ID)
	return c.refreshOAuth2Token(ctx, owner)
}

// retryAfterUnauthorized attempts to refresh credentials after an auth error
// and returns nil if the request should be retried. For OAuth providers whose
// refresh token is revoked, it triggers interactive re-authentication and
// blocks until the user completes it (or the context is cancelled).
func (c *coordinator) retryAfterUnauthorized(ctx context.Context, snapshot config.RuntimeSnapshot, expected providerregistry.RegistrationOwner, providerCfg config.ProviderConfig) error {
	if err := c.cfg.ValidateRegistrationOwner(expected); err != nil {
		return err
	}
	switch {
	case providerCfg.OAuthToken != nil:
		slog.Debug("Received 401. Refreshing token and retrying", "provider", providerCfg.ID)
		if err := c.refreshOAuth2Token(ctx, expected); err != nil {
			var exchangeErr *oauth.TokenExchangeError
			if c.notify != nil && errors.As(err, &exchangeErr) && exchangeErr.IsRefreshTokenRevoked() {
				if err := c.cfg.ValidateRegistrationOwner(expected); err != nil {
					return err
				}
				slog.Info("Refresh token revoked, waiting for re-authentication", "provider", providerCfg.ID)
				c.notify.Publish(pubsub.CreatedEvent, notify.Notification{
					Type:       notify.TypeReAuthenticate,
					ProviderID: providerCfg.ID,
					Owner:      expected,
				})
				return c.waitForInteractiveReauth(ctx, expected)
			}
			return err
		}
		return c.cfg.ValidateRegistrationOwner(expected)
	case strings.Contains(providerCfg.APIKeyTemplate, "$"):
		slog.Debug("Received 401. Refreshing API Key template and retrying", "provider", providerCfg.ID)
		return c.refreshApiKeyTemplate(ctx, snapshot, expected, providerCfg)
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
func (c *coordinator) waitForInteractiveReauth(ctx context.Context, expected providerregistry.RegistrationOwner) error {
	providerID := expected.ProviderID
	if err := c.cfg.ValidateRegistrationOwner(expected); err != nil {
		return err
	}
	waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer waitCancel()
	slog.Info("Blocking on WaitForTokenChange", "provider", providerID)
	if waitErr := c.cfg.WaitForTokenChange(waitCtx, expected); waitErr != nil {
		slog.Info("WaitForTokenChange returned error", "provider", providerID, "error", waitErr)
		return waitErr
	}
	if ctx.Err() != nil {
		slog.Warn("Original context cancelled during auth wait, cannot retry",
			"provider", providerID, "ctx_err", ctx.Err())
		return ctx.Err()
	}
	if err := c.cfg.ValidateRegistrationOwner(expected); err != nil {
		return err
	}
	if updateErr := c.UpdateModels(waitCtx); updateErr != nil {
		slog.Error("Failed to update models after re-authentication", "provider", providerID)
		return updateErr
	}
	if err := c.cfg.ValidateRegistrationOwner(expected); err != nil {
		return err
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
func (c *coordinator) makeAuthRefreshCallback(snapshot config.RuntimeSnapshot, providerCfg config.ProviderConfig) func(context.Context, *fantasy.ProviderError) error {
	if providerCfg.OAuthToken == nil &&
		!strings.Contains(providerCfg.APIKeyTemplate, "$") {
		return nil
	}
	expected, ok := snapshot.ProviderOwnerFor(providerCfg.ID, providerCfg)
	if !ok {
		return func(context.Context, *fantasy.ProviderError) error {
			return fmt.Errorf("registration owner for provider %s is not active in the admitted runtime", providerCfg.ID)
		}
	}
	return func(ctx context.Context, _ *fantasy.ProviderError) error {
		return c.retryAfterUnauthorized(ctx, snapshot, expected, providerCfg)
	}
}

func (c *coordinator) refreshOAuth2Token(ctx context.Context, expected providerregistry.RegistrationOwner) error {
	if err := c.cfg.ValidateRegistrationOwner(expected); err != nil {
		return err
	}
	if _, err := c.cfg.RefreshOAuthTokenForOwner(ctx, config.ScopeGlobal, expected); err != nil {
		slog.Error("Failed to refresh OAuth token after 401 error", "provider", expected.ProviderID)
		return err
	}
	if err := c.cfg.ValidateRegistrationOwner(expected); err != nil {
		return err
	}
	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return c.cfg.ValidateRegistrationOwner(expected)
}

func (c *coordinator) refreshApiKeyTemplate(ctx context.Context, snapshot config.RuntimeSnapshot, expected providerregistry.RegistrationOwner, providerCfg config.ProviderConfig) error {
	if err := c.cfg.ValidateRegistrationOwner(expected); err != nil {
		return err
	}
	newAPIKey, err := snapshot.Resolve(providerCfg.APIKeyTemplate)
	if err != nil {
		slog.Error("Failed to re-resolve API key after 401 error", "provider", providerCfg.ID)
		return err
	}
	if err := c.cfg.ValidateRegistrationOwner(expected); err != nil {
		return err
	}
	if err := c.cfg.SetResolvedProviderAPIKey(expected, providerCfg.APIKeyTemplate, newAPIKey); err != nil {
		return err
	}
	if err := c.cfg.ValidateRegistrationOwner(expected); err != nil {
		return err
	}
	if err := c.UpdateModels(ctx); err != nil {
		return err
	}
	return c.cfg.ValidateRegistrationOwner(expected)
}

// subAgentParams holds the parameters for running a sub-agent.
func delegatedTaskInstructions(task string) string {
	task = strings.TrimSpace(task)
	if task == "" {
		return ""
	}
	return "<delegated_task>\n" + task + "\n</delegated_task>"
}

type subAgentParams struct {
	Agent          SessionAgent
	SessionID      string
	AgentMessageID string
	ToolCallID     string
	Prompt         string
	SessionTitle   string
	ChildSessionID string
	RunApproved    bool
	// SessionSetup is an optional callback invoked after session creation
	// but before agent execution, for custom session configuration.
	SessionSetup func(sessionID string)
}

// runSubAgent runs a sub-agent and handles session management and cost accumulation.
// It creates a sub-session, runs the agent with the given prompt, and propagates
// the cost to the parent session.
func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	if params.Agent == nil {
		return fantasy.ToolResponse{}, errors.New("sub-agent is unavailable")
	}
	runtime := params.Agent.Runtime()
	cfg := runtime.Snapshot.Config()
	if cfg == nil {
		return fantasy.ToolResponse{}, errors.New("sub-agent runtime snapshot is unavailable")
	}
	model := runtime.LargeModel
	maxTokens := model.CatalogModel.DefaultMaxTokens
	if model.ModelCfg.MaxTokens != 0 {
		maxTokens = model.ModelCfg.MaxTokens
	}
	providerCfg, ok := cfg.Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return fantasy.ToolResponse{}, errModelProviderNotConfigured
	}
	providerCfg.ID = model.ModelCfg.Provider
	registration, registered := runtime.Snapshot.ProviderBehaviorRegistration(model.ModelCfg.Provider, providerCfg)
	providerOptions, err := getProviderOptions(model, providerCfg, registration)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to prepare provider options: %s", err)), nil
	}
	providerOptions = c.oauthReasoningOptionsForRegistration(model.ModelCfg.Provider, model.ModelCfg.Model, registration, registered, providerOptions)

	var session session.Session
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

	// Run the agent
	run := func() (*fantasy.AgentResult, error) {
		return params.Agent.Run(ctx, SessionAgentCall{
			SessionID:        session.ID,
			Prompt:           params.Prompt,
			TurnInstructions: delegatedTaskInstructions(params.Prompt),
			MaxOutputTokens:  maxTokens,
			ProviderOptions:  providerOptions,
			Temperature:      model.ModelCfg.Temperature,
			TopP:             model.ModelCfg.TopP,
			TopK:             model.ModelCfg.TopK,
			FrequencyPenalty: model.ModelCfg.FrequencyPenalty,
			PresencePenalty:  model.ModelCfg.PresencePenalty,
			NonInteractive:   true,
			OnProviderWarning: func(w fantasy.CallWarning) {
				c.disableOAuthReasoningForRegistration(model.ModelCfg.Provider, model.ModelCfg.Model, registration, registered, w.Message)
			},
			OnAuthRefresh: c.makeAuthRefreshCallback(runtime.Snapshot, providerCfg),
			runtime:       &runtime,
		})
	}
	initialChildCost := session.Cost
	result, err := run()
	if err != nil {
		c.disableOAuthReasoningForRegistration(model.ModelCfg.Provider, model.ModelCfg.Model, registration, registered, err.Error())
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
	snapshot := discoverSkillSnapshot(cfg)
	return snapshot.AllSkills, snapshot.ActiveSkills
}

func selectedAgentProvider(cfg *config.Config, agent config.Agent) string {
	if agent.PrimaryModelOverride != nil {
		return agent.PrimaryModelOverride.Provider
	}
	selected, ok := cfg.Models[agent.Model]
	if !ok {
		return ""
	}
	return selected.Provider
}

func providerSkillSnapshot(snapshot skills.Snapshot, cfg *config.Config, providerID string) skills.Snapshot {
	registration, ok := cfg.ProviderBehaviorRegistration(providerID)
	if !ok || registration.Instructions == nil {
		return snapshot
	}
	return filterSkillSnapshot(snapshot, registration.Instructions.HiddenSkills)
}

func filterSkillSnapshot(snapshot skills.Snapshot, hiddenSkills []string) skills.Snapshot {
	snapshot.ActiveSkills = skills.Filter(snapshot.ActiveSkills, hiddenSkills)
	return snapshot
}

func discoverSkillSnapshot(cfg *config.ConfigStore) skills.Snapshot {
	return discoverSkillSnapshotWithRuntime(cfg, cfg.RuntimeSnapshot())
}

func discoverSkillSnapshotWithRuntime(store *config.ConfigStore, snapshot config.RuntimeSnapshot) skills.Snapshot {
	opts := snapshot.Config().Options
	var paths, disabled []string
	if opts != nil {
		paths = opts.SkillsPaths
		disabled = opts.DisabledSkills
	}
	discovery := skills.DiscoveryConfig{
		SkillsPaths:    paths,
		DisabledSkills: disabled,
		WorkingDir:     store.WorkingDir(),
		Resolver:       snapshot.Resolve,
	}
	allSkills, activeSkills, states := skills.DiscoverFromConfig(discovery)
	logDiscoveryStats(states, paths, allSkills, activeSkills, disabled)
	return skills.Snapshot{
		AllSkills:     allSkills,
		ActiveSkills:  activeSkills,
		States:        states,
		ResolvedPaths: discovery.ResolvePaths(),
	}
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
