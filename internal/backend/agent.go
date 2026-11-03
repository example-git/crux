package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/agent/notify"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/imageattachment"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/shell"
)

// SendMessage validates and accepts a prompt for the workspace's agent,
// then dispatches the run on a goroutine bound to the workspace context.
// By default it returns immediately. WaitForAdmission waits until the native
// user message is persisted or admission fails, but never waits for the LLM
// turn to complete. The run's lifetime is owned by the workspace, not by the
// caller. Later errors reach observers through the agent event channels.
//
// SendMessage returns synchronously when the request cannot be accepted:
// ErrWorkspaceNotFound if the workspace is missing, ErrAgentNotInitialized
// if its coordinator is nil, the structural validation errors from
// agent.ValidateCall (ErrEmptyPrompt, ErrSessionMissing) when the prompt
// or session is missing, and ErrWorkspaceClosing if the workspace is
// being torn down.
func (b *Backend) SendMessage(workspaceID string, msg proto.AgentMessage) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	coordinator := ws.CurrentAgentCoordinator()
	if coordinator == nil {
		return ErrAgentNotInitialized
	}

	deliveryMode := agent.DeliveryMode(msg.DeliveryMode)
	attachments := proto.AttachmentsToMessage(msg.Attachments)
	var inputParts []message.ContentPart
	var inputMaterializer agent.InputMaterializer
	if deliveryMode == agent.DeliverySteer {
		if err := validateAgentInputParts(msg.InputParts); err != nil {
			return err
		}
		if msg.InputParts != nil {
			rawInputParts := append([]proto.AgentInputPart(nil), msg.InputParts...)
			inputMaterializer = func(ctx context.Context) ([]message.ContentPart, []message.Attachment, error) {
				return b.materializeAgentInputParts(ctx, ws, rawInputParts)
			}
		}
	} else {
		var skillAttachments []message.Attachment
		inputParts, skillAttachments, err = b.materializeAgentInputParts(ws.ctx, ws, msg.InputParts)
		if err != nil {
			return err
		}
		attachments = append(attachments, skillAttachments...)
	}
	if err := agent.ValidateCall(agent.SessionAgentCall{
		SessionID:         msg.SessionID,
		RunID:             msg.RunID,
		DeliveryMode:      deliveryMode,
		RequireActiveRun:  msg.RequireActiveRun,
		Prompt:            msg.Prompt,
		Attachments:       attachments,
		InputParts:        inputParts,
		InputMaterializer: inputMaterializer,
	}); err != nil {
		return err
	}
	if msg.PermissionMode != proto.AgentPermissionInteractive && msg.PermissionMode != proto.AgentPermissionDeny && msg.PermissionMode != proto.AgentPermissionBypass {
		return fmt.Errorf("invalid agent permission mode %q", msg.PermissionMode)
	}
	accept := coordinator.BeginAccepted(msg.SessionID)

	ws.runMu.Lock()
	if ws.closing {
		ws.runMu.Unlock()
		accept.Close()
		return ErrWorkspaceClosing
	}
	ws.runWG.Add(1)
	ws.runMu.Unlock()

	var admission chan error
	var resolveAdmission func(error)
	if msg.WaitForAdmission {
		admission = make(chan error, 1)
		var once sync.Once
		resolveAdmission = func(err error) {
			once.Do(func() { admission <- err })
		}
	}
	go b.runAgent(ws, coordinator, msg, inputParts, inputMaterializer, attachments, accept, resolveAdmission)
	if admission != nil {
		return <-admission
	}
	return nil
}

func validateAgentInputParts(input []proto.AgentInputPart) error {
	for _, part := range input {
		switch part.Type {
		case "text":
		case "local_file":
			if part.Path == "" {
				return errors.New("local file path must not be empty")
			}
		case "skill":
			if part.Path == "" {
				return errors.New("skill path must not be empty")
			}
			if part.Text == "" {
				return errors.New("skill name must not be empty")
			}
		default:
			return fmt.Errorf("unsupported agent input part type %q", part.Type)
		}
	}
	return nil
}

func (b *Backend) materializeAgentInputParts(ctx context.Context, ws *Workspace, input []proto.AgentInputPart) ([]message.ContentPart, []message.Attachment, error) {
	if input == nil {
		return nil, nil, nil
	}
	parts := make([]message.ContentPart, 0, len(input))
	var attachments []message.Attachment
	for _, part := range input {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		switch part.Type {
		case "text":
			parts = append(parts, message.TextContent{Text: part.Text, TextElements: part.TextElements})
		case "local_file":
			content, err := readWorkspaceImage(ctx, ws, part.Path)
			if err != nil {
				return nil, nil, err
			}
			parts = append(parts, content)
		case "skill":
			if part.Path == "" {
				return nil, nil, errors.New("skill path must not be empty")
			}
			if part.Text == "" {
				return nil, nil, errors.New("skill name must not be empty")
			}
			content, result, err := b.ReadSkill(ctx, ws.ID, part.Path)
			if err != nil {
				return nil, nil, err
			}
			if result.Name != part.Text {
				return nil, nil, fmt.Errorf("skill name %q does not match selected skill %q", part.Text, result.Name)
			}
			parts = append(parts, message.SkillContent{Name: result.Name, Path: part.Path, Content: content})
		default:
			return nil, nil, fmt.Errorf("unsupported agent input part type %q", part.Type)
		}
	}
	return parts, attachments, nil
}

func readWorkspaceImage(ctx context.Context, ws *Workspace, path string) (message.BinaryContent, error) {
	if err := ctx.Err(); err != nil {
		return message.BinaryContent{}, err
	}
	if path == "" {
		return message.BinaryContent{}, errors.New("local file path must not be empty")
	}
	rootPath, err := fsext.CanonicalPath(ws.resolvedPath)
	if err != nil {
		return message.BinaryContent{}, fmt.Errorf("resolve workspace root: %w", err)
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(rootPath, candidate)
	}
	resolved, err := fsext.CanonicalPath(candidate)
	if err != nil {
		return message.BinaryContent{}, fmt.Errorf("resolve local file %q: %w", path, err)
	}
	relative, err := filepath.Rel(rootPath, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return message.BinaryContent{}, fmt.Errorf("local file %q is outside the workspace", path)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return message.BinaryContent{}, fmt.Errorf("open workspace root: %w", err)
	}
	defer root.Close()
	file, err := root.Open(relative)
	if err != nil {
		return message.BinaryContent{}, fmt.Errorf("open local file %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return message.BinaryContent{}, fmt.Errorf("inspect local file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return message.BinaryContent{}, fmt.Errorf("local file %q is not a regular file", path)
	}
	if info.Size() > imageattachment.MaxSourceBytes {
		return message.BinaryContent{}, fmt.Errorf("local file %q exceeds %d bytes", path, imageattachment.MaxSourceBytes)
	}
	if err := ctx.Err(); err != nil {
		return message.BinaryContent{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, imageattachment.MaxSourceBytes+1))
	if err != nil {
		return message.BinaryContent{}, fmt.Errorf("read local file %q: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return message.BinaryContent{}, err
	}
	if int64(len(data)) > imageattachment.MaxSourceBytes {
		return message.BinaryContent{}, fmt.Errorf("local file %q exceeds %d bytes", path, imageattachment.MaxSourceBytes)
	}
	mediaType := http.DetectContentType(data)
	if !strings.HasPrefix(mediaType, "image/") {
		return message.BinaryContent{}, fmt.Errorf("local file %q is not an image", path)
	}
	return message.BinaryContent{Path: resolved, MIMEType: mediaType, Data: data}, nil
}

// runAgent executes an accepted agent run for the workspace. It owns the
// accept reservation (releasing it on return) and the runWG ticket added
// by SendMessage. The run is bound to the workspace context so its
// lifetime is independent of any client's HTTP request.
//
// On a non-cancel error it surfaces the failure to observers via a
// notify.TypeAgentError notification (lossy, best-effort). That alone is
// not a reliable terminal signal: the agent-event fan-in uses lossy
// subscribers, so a `crux run` caller blocking on its RunID could hang
// if the event is dropped. To guarantee termination, when msg.RunID is
// non-empty and the coordinator did not already publish the run's
// authoritative terminal RunComplete (e.g. the error was returned before
// sessionAgent.Run executed, such as a readyWg or UpdateModels failure),
// runAgent emits an errored RunComplete on the must-deliver
// runCompletions broker so the waiter observes a deterministic terminal
// event. context.Canceled is expected (sessionAgent.Run already
// publishes the cancelled terminal marker) and produces no error
// terminal event.
//
// When msg.RunID is non-empty it is attached to the context via
// agent.WithRunID so the coordinator can stamp the terminal
// notify.RunComplete event with that correlator. A run-complete marker
// is also attached so the coordinator can report whether it published
// the terminal event, letting runAgent avoid a duplicate fallback.
func (b *Backend) runAgent(ws *Workspace, coordinator agent.Coordinator, msg proto.AgentMessage, inputParts []message.ContentPart, inputMaterializer agent.InputMaterializer, attachments []message.Attachment, accept *agent.AcceptedRun, resolveAdmission func(error)) {
	defer ws.runWG.Done()
	defer accept.Close()

	ctx := agent.WithDeliveryMode(ws.ctx, agent.DeliveryMode(msg.DeliveryMode))
	if msg.RequireActiveRun {
		ctx = agent.WithRequireActiveRun(ctx)
	}
	switch msg.PermissionMode {
	case proto.AgentPermissionBypass:
		ctx = permission.WithRunApproval(ctx)
	case proto.AgentPermissionDeny:
		ctx = permission.WithDetachedAgent(ctx)
	}
	if msg.SubmissionID != "" {
		ctx = agent.WithSubmissionID(ctx, msg.SubmissionID)
	}
	if msg.RunID != "" {
		ctx = agent.WithRunID(ctx, msg.RunID)
	}
	messageRunID := msg.MessageRunID
	if messageRunID == "" {
		messageRunID = msg.RunID
	}
	if messageRunID != "" {
		ctx = agent.WithMessageRunID(ctx, messageRunID)
	}
	if msg.UserMessageID != "" {
		ctx = agent.WithUserMessageID(ctx, msg.UserMessageID)
	}
	if msg.AssistantItemID != "" {
		ctx = agent.WithAssistantItemID(ctx, msg.AssistantItemID)
	}
	if msg.TurnInstructions != "" {
		ctx = agent.WithTurnInstructions(ctx, msg.TurnInstructions)
	}
	if msg.InstructionIsolation {
		ctx = agent.WithInstructionIsolation(ctx)
	}
	if len(msg.InstructionIsolationTools) > 0 {
		ctx = agent.WithInstructionIsolationTools(ctx, msg.InstructionIsolationTools)
	}
	if msg.AnalysisEffort != "" {
		ctx = agent.WithAnalysisEffort(ctx, msg.AnalysisEffort)
	}
	if msg.ProviderOperation != "" {
		ctx = agent.WithProviderOperation(ctx, msg.ProviderOperation)
	}
	if msg.InputParts != nil && inputMaterializer == nil {
		ctx = agent.WithInputParts(ctx, inputParts)
	}
	if inputMaterializer != nil {
		ctx = agent.WithInputMaterializer(ctx, inputMaterializer)
	}
	ctx = agent.WithRunCompleteMarker(ctx)
	if resolveAdmission != nil {
		ctx = agent.WithAdmissionCallback(ctx, resolveAdmission)
	}

	_, err := coordinator.RunAccepted(ctx, accept, msg.SessionID, msg.Prompt, attachments...)
	if resolveAdmission != nil && err != nil {
		resolveAdmission(err)
	}
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}

	eventRunID := msg.RunID
	if eventRunID == "" {
		eventRunID = msg.MessageRunID
	}
	ws.AgentNotifications().Publish(pubsub.CreatedEvent, notify.Notification{
		SessionID: msg.SessionID,
		RunID:     eventRunID,
		Type:      notify.TypeAgentError,
		Message:   err.Error(),
	})

	if msg.RunID == "" || agent.RunCompletePublished(ctx) {
		return
	}
	if rc := ws.RunCompletions(); rc != nil {
		rc.PublishMustDeliver(ctx, pubsub.UpdatedEvent, notify.RunComplete{
			SessionID: msg.SessionID,
			RunID:     msg.RunID,
			Error:     err.Error(),
		})
	}
}

// GetAgentInfo returns the agent's model and busy status.
func (b *Backend) GetAgentInfo(workspaceID string) (proto.AgentInfo, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.AgentInfo{}, err
	}

	var agentInfo proto.AgentInfo
	if coordinator := ws.CurrentAgentCoordinator(); coordinator != nil {
		m := coordinator.Model()
		agentInfo = proto.AgentInfo{
			Model:    m.CatalogModel,
			ModelCfg: m.ModelCfg,
			IsBusy:   coordinator.IsBusy(),
			IsReady:  true,
		}
	}
	return agentInfo, nil
}

func (b *Backend) GetAgentInstructions(ctx context.Context, workspaceID string) (agent.InstructionSnapshot, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return agent.InstructionSnapshot{}, err
	}
	coordinator := ws.CurrentAgentCoordinator()
	if coordinator == nil {
		return agent.InstructionSnapshot{}, ErrAgentNotInitialized
	}
	return agent.CurrentInstructionSnapshot(ctx, coordinator)
}

// InitAgent initializes the coder agent for the workspace.
func (b *Backend) InitAgent(ctx context.Context, workspaceID string, interactive bool) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	if interactive {
		return ws.InitCoderAgent(ctx)
	}
	return ws.InitCoderAgentNonInteractive(ctx)
}

// UpdateAgent reloads the agent model configuration.
func (b *Backend) UpdateAgent(ctx context.Context, workspaceID string, expected config.AgentModelState) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	return ws.UpdateAgentModel(ctx, expected)
}

func (b *Backend) CreateAgentDefinition(workspaceID string, request proto.CreateAgentDefinitionRequest) (string, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return "", err
	}
	template := agent.AgentDefinitionTemplate{
		Scope:        agent.AgentDefinitionScope(request.Scope),
		Name:         request.Name,
		Description:  request.Description,
		Instructions: request.Instructions,
		Model:        request.Model,
		Tools:        request.Tools,
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
	return agent.CreateAgentDefinition(ws.Path, ws.Cfg.Config(), template)
}

// CancelSession cancels an ongoing agent operation for the given
// session.
func (b *Backend) CancelSession(workspaceID, sessionID string) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	if coordinator := ws.CurrentAgentCoordinator(); coordinator != nil {
		coordinator.Cancel(sessionID)
	}
	return nil
}

// SummarizeSession triggers a session summarization.
func (b *Backend) SummarizeSession(ctx context.Context, workspaceID, sessionID string) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	coordinator := ws.CurrentAgentCoordinator()
	if coordinator == nil {
		return ErrAgentNotInitialized
	}

	return coordinator.Summarize(ctx, sessionID)
}

// RewindSession rewinds a session to a given message, optionally
// summarizing the conversation first.
func (b *Backend) RewindSession(ctx context.Context, workspaceID, sessionID, messageID string, summarize, restoreFiles bool) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	return ws.RewindSession(ctx, sessionID, messageID, summarize, restoreFiles)
}

func (b *Backend) RollbackSession(ctx context.Context, workspaceID, sessionID, messageID string) (message.RollbackResult, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return message.RollbackResult{}, err
	}
	return ws.RollbackSession(ctx, sessionID, messageID)
}

// SuggestPrompt predicts the user's likely next message for a session.
func (b *Backend) SuggestPrompt(ctx context.Context, workspaceID, sessionID string) (string, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return "", err
	}

	coordinator := ws.CurrentAgentCoordinator()
	if coordinator == nil {
		return "", ErrAgentNotInitialized
	}

	return coordinator.SuggestPrompt(ctx, sessionID)
}

// DetachForegroundJobs sends commands a bash tool call is waiting on
// to the background. Returns the number of commands detached.
func (b *Backend) ForegroundTaskControl(workspaceID, sessionID string, detach bool) (int, error) {
	workspace, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return 0, err
	}
	if detach {
		return workspace.BackgroundShells.ForegroundWaits.Detach(sessionID), nil
	}
	return workspace.BackgroundShells.ForegroundWaits.Count(sessionID), nil
}

func (b *Backend) DetachForegroundJobs(workspaceID string) (int, error) {
	workspace, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return 0, err
	}
	return workspace.BackgroundShells.DetachForeground(), nil
}

// QueuedPrompts returns the number of queued prompts for the session.
func (b *Backend) QueuedPrompts(workspaceID, sessionID string) (int, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return 0, err
	}

	coordinator := ws.CurrentAgentCoordinator()
	if coordinator == nil {
		return 0, nil
	}

	return coordinator.QueuedPrompts(sessionID), nil
}

// ClearQueue clears the prompt queue for the session.
func (b *Backend) ClearQueue(workspaceID, sessionID string) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	if coordinator := ws.CurrentAgentCoordinator(); coordinator != nil {
		coordinator.ClearQueue(sessionID)
	}
	return nil
}

// QueuedPromptsList returns the list of queued prompt strings for a
// session.
func (b *Backend) QueuedPromptsList(workspaceID, sessionID string) ([]proto.QueuedPrompt, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	coordinator := ws.CurrentAgentCoordinator()
	if coordinator == nil {
		return nil, nil
	}

	queued := coordinator.QueuedPromptsList(sessionID)
	prompts := make([]proto.QueuedPrompt, len(queued))
	for i, prompt := range queued {
		prompts[i] = proto.QueuedPrompt{
			DeliveryMode: string(prompt.DeliveryMode),
			SubmissionID: prompt.SubmissionID,
			Prompt:       prompt.Prompt,
		}
	}
	return prompts, nil
}

// GetDefaultSmallModel returns the default small model for a provider.
func (b *Backend) GetDefaultSmallModel(workspaceID, providerID string) (config.SelectedModel, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return config.SelectedModel{}, err
	}

	return ws.GetDefaultSmallModel(providerID)
}

// RunShellCommand runs a shell command in the workspace directory and
// persists the command + output as a user message in the session.
func (b *Backend) RunShellCommand(ctx context.Context, workspaceID string, req proto.ShellCommandRequest) (proto.ShellCommandResponse, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.ShellCommandResponse{}, err
	}
	ctx, complete, err := ws.beginCredentialOperation(ctx)
	if err != nil {
		return proto.ShellCommandResponse{}, err
	}
	defer complete()

	var persist shell.PersistFunc
	if req.SessionID != "" {
		persist = func(cmd, output string, exitCode int) error {
			return shell.PersistOutput(ctx, ws.Messages, req.SessionID, cmd, output, exitCode)
		}
	}

	result, err := shell.RunAndPersist(ctx, shell.RunOptions{
		Command:   req.Command,
		Cwd:       ws.Path,
		Env:       append(ws.Cfg.Environment(), ws.Env...),
		TermWidth: req.TermWidth,
	}, persist)
	if err != nil {
		return proto.ShellCommandResponse{}, err
	}

	return proto.ShellCommandResponse{
		Output:   result.Output,
		ExitCode: result.ExitCode,
	}, nil
}
