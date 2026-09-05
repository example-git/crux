package agent

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	fantasy "github.com/example-git/crux/foundation"

	"github.com/example-git/crux/internal/agent/prompt"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
	managedtask "github.com/example-git/crux/internal/task"
)

//go:embed templates/agent_tool.md
var agentToolDescription string

type AgentParams struct {
	Prompt          string `json:"prompt" description:"The task for the agent to perform"`
	SubagentType    string `json:"subagent_type,omitempty" description:"Optional preset subagent type. Omit this to use the generic task agent."`
	RunInBackground bool   `json:"run_in_background,omitempty" description:"Set to true to run this agent in the background. Multiple background agents may run concurrently, and you will be notified when each completes."`
	ContinueTaskID  string `json:"continue_task_id,omitempty" description:"Continue a terminal background-agent transcript by task ID."`
}

type AgentResponseMetadata struct {
	Background     bool   `json:"background"`
	TaskID         string `json:"task_id,omitempty"`
	ChildSessionID string `json:"child_session_id,omitempty"`
}

const (
	AgentToolName = "agent"
)

type presetSubagent struct {
	agent            SessionAgent
	title            string
	tools            []string
	requiresApproval bool
}

func (c *coordinator) agentTool(ctx context.Context, promptTemplate *prompt.Prompt) (fantasy.AgentTool, error) {
	return c.agentToolWithSnapshot(ctx, promptTemplate, c.cfg.RuntimeSnapshot())
}

func (c *coordinator) agentToolWithSnapshot(ctx context.Context, promptTemplate *prompt.Prompt, snapshot config.RuntimeSnapshot) (fantasy.AgentTool, error) {
	cfg := snapshot.Config()
	taskCfg, ok := cfg.Agents[config.AgentTask]
	if !ok {
		return nil, errors.New("task agent not configured")
	}
	taskAgent, err := c.buildAgentWithSnapshot(ctx, promptTemplate, taskCfg, true, snapshot)
	if err != nil {
		return nil, err
	}
	taskPreset := presetSubagent{
		agent: taskAgent,
		title: "New Agent Session",
	}
	return newRefreshingAgentTool(c, taskCfg, taskPreset), nil
}

type refreshingAgentTool struct {
	coordinator *coordinator
	taskConfig  config.Agent
	inner       fantasy.AgentTool
}

func newRefreshingAgentTool(
	c *coordinator,
	taskCfg config.Agent,
	taskPreset presetSubagent,
) fantasy.AgentTool {
	tool := &refreshingAgentTool{
		coordinator: c,
		taskConfig:  taskCfg,
	}
	tool.inner = fantasy.NewParallelAgentTool(
		AgentToolName,
		"",
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return c.runAgentTool(ctx, params, call, taskPreset)
		},
	)
	return tool
}

func (t *refreshingAgentTool) Info() fantasy.ToolInfo {
	info := t.inner.Info()
	info.Description = t.coordinator.currentAgentToolDescription(t.taskConfig)
	return info
}

func (t *refreshingAgentTool) Run(
	ctx context.Context,
	call fantasy.ToolCall,
) (fantasy.ToolResponse, error) {
	return t.inner.Run(ctx, call)
}

func (t *refreshingAgentTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *refreshingAgentTool) SetProviderOptions(options fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(options)
}

func (c *coordinator) currentAgentDefinitions() ([]agentDefinition, error) {
	if c.loadAgentDefinitions == nil {
		return nil, nil
	}
	return c.loadAgentDefinitions(c.cfg.WorkingDir(), c.cfg.Config())
}

func (c *coordinator) currentAgentToolDescription(taskCfg config.Agent) string {
	descriptionLines := []string{
		fmt.Sprintf(
			"- %s: %s (model: configured large model; tools: %s)",
			config.AgentTask,
			taskCfg.Description,
			strings.Join(resolveSubagentTools(taskCfg, c.disabledTools()), ", "),
		),
	}
	definitions, err := c.currentAgentDefinitions()
	if err != nil {
		descriptionLines = append(
			descriptionLines,
			fmt.Sprintf("- custom agent definitions unavailable: %v", err),
		)
	} else {
		for _, definition := range definitions {
			descriptionLines = append(
				descriptionLines,
				c.agentDefinitionDescription(definition),
			)
		}
	}
	return strings.TrimSpace(agentToolDescription) +
		"\n\nAvailable subagent types:\n" +
		strings.Join(descriptionLines, "\n")
}

func (c *coordinator) agentDefinitionDescription(definition agentDefinition) string {
	if definition.ValidationErr != nil {
		return fmt.Sprintf(
			"- %s: invalid definition at %s",
			definition.Name,
			definition.Path,
		)
	}
	allowedTools := c.customAgentTools(definition)
	toolSummary := "none"
	if len(allowedTools) > 0 {
		toolSummary = strings.Join(allowedTools, ", ")
	}
	return fmt.Sprintf(
		"- %s: %s (model: %s/%s; tools: %s)",
		definition.Name,
		definition.Description,
		definition.Model.Provider,
		definition.Model.Model,
		toolSummary,
	)
}

func (c *coordinator) customAgentTools(definition agentDefinition) []string {
	allowedTools := slices.Clone(definition.Tools)
	if definition.AllTools {
		allowedTools = c.customWildcardTools()
		if definition.Script == nil {
			allowedTools = slices.DeleteFunc(allowedTools, func(name string) bool {
				return name == tools.ScriptToolName
			})
		}
	}
	return resolveSubagentTools(config.Agent{AllowedTools: allowedTools}, c.disabledTools())
}

func (c *coordinator) disabledTools() []string {
	if c.cfg == nil || c.cfg.Config().Options == nil {
		return nil
	}
	return c.cfg.Config().Options.DisabledTools
}

func availableAgentTypes(definitions []agentDefinition) []string {
	available := make([]string, 0, len(definitions)+1)
	available = append(available, config.AgentTask)
	for _, definition := range definitions {
		available = append(available, definition.Name)
	}
	slices.Sort(available)
	return available
}

func (c *coordinator) resolveAgentToolPreset(
	ctx context.Context,
	taskPreset presetSubagent,
	requested string,
) (presetSubagent, error) {
	definitions, err := c.currentAgentDefinitions()
	if err != nil {
		return presetSubagent{}, err
	}
	selectedType := requested
	if selectedType == "" {
		selectedType = config.AgentTask
	}
	if selectedType == config.AgentTask {
		return taskPreset, nil
	}
	for _, definition := range definitions {
		if definition.Name != selectedType {
			continue
		}
		if definition.ValidationErr != nil {
			return presetSubagent{}, definition.ValidationErr
		}
		return c.buildCustomAgentPreset(ctx, definition)
	}
	return presetSubagent{}, fmt.Errorf(
		"unknown subagent type %q; available types: %s",
		requested,
		strings.Join(availableAgentTypes(definitions), ", "),
	)
}

func (c *coordinator) buildCustomAgentPreset(
	ctx context.Context,
	definition agentDefinition,
) (presetSubagent, error) {
	allowedTools := c.customAgentTools(definition)
	model := definition.Model
	agentCfg := config.Agent{
		ID:                   definition.Name,
		Name:                 definition.Name,
		Description:          definition.Description,
		Model:                config.SelectedModelTypeLarge,
		PrimaryModelOverride: &model,
		Instructions:         definition.Instructions,
		DefinitionPath:       definition.Path,
		AllowAllTools:        definition.AllTools,
		Script:               definition.Script,
		AllowedTools:         allowedTools,
		AllowedMCP:           map[string][]string{},
	}
	promptTemplate, err := c.currentSubagentPromptTemplate()
	if err != nil {
		return presetSubagent{}, err
	}
	customAgent, err := c.buildAgent(ctx, promptTemplate, agentCfg, true)
	if err != nil {
		return presetSubagent{}, fmt.Errorf(
			"building custom agent from %q: %w",
			definition.Path,
			err,
		)
	}
	return presetSubagent{
		agent:            customAgent,
		title:            "Agent: " + definition.Name,
		tools:            allowedTools,
		requiresApproval: subagentRequiresApproval(allowedTools),
	}, nil
}

func (c *coordinator) runAgentTool(
	ctx context.Context,
	params AgentParams,
	call fantasy.ToolCall,
	taskPreset presetSubagent,
) (fantasy.ToolResponse, error) {
	if permission.IsSubagent(ctx) {
		return fantasy.NewTextErrorResponse(permission.ErrSubagentLaunch.Error()), nil
	}
	if params.Prompt == "" {
		return fantasy.NewTextErrorResponse("prompt is required"), nil
	}
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, errors.New("session id missing from context")
	}
	agentMessageID := tools.GetMessageFromContext(ctx)
	if agentMessageID == "" {
		return fantasy.ToolResponse{}, errors.New("agent message id missing from context")
	}
	var continuation *BackgroundAgentTask
	effectiveAgentType := params.SubagentType
	var err error
	if params.ContinueTaskID != "" {
		if !params.RunInBackground {
			return fantasy.NewTextErrorResponse("continue_task_id requires run_in_background=true"), nil
		}
		continuation, effectiveAgentType, err = c.resolveBackgroundAgentContinuation(
			ctx,
			params.ContinueTaskID,
			effectiveAgentType,
		)
		if err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	selected, err := c.resolveAgentToolPreset(
		ctx,
		taskPreset,
		effectiveAgentType,
	)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	params.SubagentType = effectiveAgentType
	runParams := subAgentParams{
		Agent:          selected.agent,
		SessionID:      sessionID,
		AgentMessageID: agentMessageID,
		ToolCallID:     call.ID,
		Prompt:         params.Prompt,
		SessionTitle:   selected.title,
	}
	if continuation != nil {
		return c.continueBackgroundSubAgent(
			ctx,
			params,
			call,
			selected,
			runParams,
			continuation,
		)
	}
	if !params.RunInBackground {
		return c.runSubAgent(ctx, runParams)
	}
	return c.startBackgroundSubAgent(
		ctx,
		params,
		call,
		selected,
		runParams,
	)
}

func (c *coordinator) buildContinuationAgent(
	ctx context.Context,
	agentType string,
) (presetSubagent, error) {
	cfg := c.cfg.Config()
	if agentType == config.AgentTask {
		taskCfg, ok := cfg.Agents[config.AgentTask]
		if !ok {
			return presetSubagent{}, errors.New("task agent not configured")
		}
		template, err := c.currentSubagentPromptTemplate()
		if err != nil {
			return presetSubagent{}, err
		}
		built, err := c.buildAgent(ctx, template, taskCfg, true)
		if err != nil {
			return presetSubagent{}, err
		}
		return presetSubagent{
			agent: built,
			title: "New Agent Session",
		}, nil
	}
	definitions, err := c.currentAgentDefinitions()
	if err != nil {
		return presetSubagent{}, err
	}
	for _, definition := range definitions {
		if definition.Name != agentType {
			continue
		}
		if definition.ValidationErr != nil {
			return presetSubagent{}, definition.ValidationErr
		}
		return c.buildCustomAgentPreset(ctx, definition)
	}
	return presetSubagent{}, fmt.Errorf(
		"unknown subagent_type %q",
		agentType,
	)
}

func (c *coordinator) ContinueTask(ctx context.Context, taskID, parentSessionID, continuationPrompt, originToolCallID string) (managedtask.View, error) {
	if permission.IsSubagent(ctx) {
		return managedtask.View{}, permission.ErrSubagentBackgroundTask
	}
	if continuationPrompt == "" {
		return managedtask.View{}, errors.New("prompt is required")
	}
	if parentSessionID == "" {
		return managedtask.View{}, errors.New("parent session ID is required")
	}
	continuation, agentType, err := c.resolveBackgroundAgentContinuation(ctx, taskID, "")
	if err != nil {
		return managedtask.View{}, err
	}
	info := continuation.Info()
	if info.Ownership.ParentSessionID != parentSessionID {
		return managedtask.View{}, fmt.Errorf("background agent task does not belong to parent session %s", parentSessionID)
	}
	selected, err := c.buildContinuationAgent(ctx, agentType)
	if err != nil {
		return managedtask.View{}, err
	}
	approved, err := c.approveBackgroundSubagent(ctx, selected, parentSessionID, originToolCallID)
	if err != nil {
		return managedtask.View{}, err
	}
	backgroundTask, err := c.startBackgroundAgentContinuation(ctx, continuationPrompt, originToolCallID, selected, subAgentParams{
		Agent:          selected.agent,
		SessionID:      parentSessionID,
		ToolCallID:     originToolCallID,
		Prompt:         continuationPrompt,
		SessionTitle:   selected.title,
		ChildSessionID: info.ChildSessionID,
		RunApproved:    approved,
	}, continuation)
	if err != nil {
		return managedtask.View{}, err
	}
	return managedAgentInfo(backgroundTask.Info()), nil
}

func (c *coordinator) resolveBackgroundAgentContinuation(ctx context.Context, taskID, requestedAgentType string) (*BackgroundAgentTask, string, error) {
	if c.backgroundAgents == nil {
		return nil, "", fmt.Errorf("background agent task not found: %s", taskID)
	}
	continuation, err := c.backgroundAgents.task(taskID)
	if err != nil {
		return nil, "", err
	}
	info := continuation.Info()
	if !info.State.Status.Terminal() {
		return nil, "", fmt.Errorf("background agent task is not terminal: %s", taskID)
	}
	if info.ChildSessionID == "" {
		return nil, "", fmt.Errorf("background agent transcript is unavailable: %s", taskID)
	}
	child, err := c.sessions.Get(ctx, info.ChildSessionID)
	if err != nil || child.ParentSessionID != info.Ownership.ParentSessionID {
		return nil, "", fmt.Errorf("background agent transcript is unavailable: %s", taskID)
	}
	if requestedAgentType != "" && requestedAgentType != info.AgentType {
		return nil, "", fmt.Errorf("subagent_type %q does not match continuation agent type %q", requestedAgentType, info.AgentType)
	}
	return continuation, info.AgentType, nil
}

func (c *coordinator) startBackgroundAgentContinuation(ctx context.Context, continuationPrompt, originToolCallID string, selected presetSubagent, runParams subAgentParams, continuation *BackgroundAgentTask) (*BackgroundAgentTask, error) {
	info := continuation.Info()
	usageBaseline := c.backgroundAgentUsage(info.ChildSessionID)
	backgroundTask, err := c.backgroundAgents.ReserveContinuationContext(ctx, continuationPrompt, info.AgentType, selected.title, info.ID, usageBaseline, managedtask.Ownership{
		ParentSessionID:  runParams.SessionID,
		OriginToolCallID: originToolCallID,
	})
	if err != nil {
		return nil, err
	}
	runParams.ChildSessionID = info.ChildSessionID
	start := c.backgroundAgents.Start
	if runParams.RunApproved {
		start = c.backgroundAgents.StartApproved
	}
	if err := start(backgroundTask, info.ChildSessionID, func(runCtx context.Context) backgroundAgentResult {
		response, runErr := c.runSubAgent(runCtx, runParams)
		result := backgroundAgentResult{Err: runErr}
		if runErr == nil {
			if response.IsError {
				result.Err = errors.New(response.Content)
			} else {
				result.Output = response.Content
			}
		}
		result.Usage = agentUsageSince(c.backgroundAgentUsage(info.ChildSessionID), usageBaseline)
		return result
	}); err != nil {
		return nil, fmt.Errorf("continuing background agent: %w", err)
	}
	return backgroundTask, nil
}

func (c *coordinator) continueBackgroundSubAgent(ctx context.Context, params AgentParams, call fantasy.ToolCall, selected presetSubagent, runParams subAgentParams, continuation *BackgroundAgentTask) (fantasy.ToolResponse, error) {
	approved, err := c.approveBackgroundSubagent(ctx, selected, runParams.SessionID, call.ID)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	runParams.RunApproved = approved
	backgroundTask, err := c.startBackgroundAgentContinuation(ctx, params.Prompt, call.ID, selected, runParams, continuation)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	metadata := AgentResponseMetadata{
		Background:     true,
		TaskID:         backgroundTask.ID,
		ChildSessionID: continuation.Info().ChildSessionID,
	}
	response := fmt.Sprintf("Background agent continued with ID: %s\n\nChild session ID: %s", backgroundTask.ID, metadata.ChildSessionID)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
}

func (c *coordinator) startBackgroundSubAgent(ctx context.Context, params AgentParams, call fantasy.ToolCall, selected presetSubagent, runParams subAgentParams) (fantasy.ToolResponse, error) {
	approved, err := c.approveBackgroundSubagent(ctx, selected, runParams.SessionID, call.ID)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	runParams.RunApproved = approved
	if c.backgroundAgents == nil {
		c.backgroundAgents, err = NewBackgroundAgentManagerWithStore(c.cfg.WorkingDir(), c.backgroundShells, nil)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("initialize global background agent admission: %v", err)), nil
		}
	}
	agentType := params.SubagentType
	if agentType == "" {
		agentType = config.AgentTask
	}
	backgroundTask, err := c.backgroundAgents.ReserveContext(ctx, params.Prompt, agentType, selected.title, managedtask.Ownership{
		ParentSessionID:  runParams.SessionID,
		OriginToolCallID: call.ID,
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	childSessionID := c.sessions.CreateAgentToolSessionID(runParams.AgentMessageID, runParams.ToolCallID)
	childSession, err := c.sessions.CreateTaskSession(ctx, childSessionID, runParams.SessionID, runParams.SessionTitle)
	if err != nil {
		c.backgroundAgents.FailReservation(backgroundTask, fmt.Errorf("create session: %w", err))
		return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
	}
	runParams.ChildSessionID = childSession.ID
	start := c.backgroundAgents.Start
	if runParams.RunApproved {
		start = c.backgroundAgents.StartApproved
	}
	if err := start(backgroundTask, childSession.ID, func(runCtx context.Context) backgroundAgentResult {
		response, runErr := c.runSubAgent(runCtx, runParams)
		result := backgroundAgentResult{Err: runErr}
		if runErr == nil {
			if response.IsError {
				result.Err = errors.New(response.Content)
			} else {
				result.Output = response.Content
			}
		}
		result.Usage = c.backgroundAgentUsage(childSession.ID)
		return result
	}); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("starting background agent: %w", err)
	}
	metadata := AgentResponseMetadata{
		Background:     true,
		TaskID:         backgroundTask.ID,
		ChildSessionID: childSession.ID,
	}
	response := fmt.Sprintf("Background agent started with ID: %s\n\nChild session ID: %s", backgroundTask.ID, metadata.ChildSessionID)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
}

func (c *coordinator) backgroundAgentUsage(childSessionID string) managedtask.AgentUsage {
	usage := managedtask.AgentUsage{}
	if persisted, err := c.sessions.Get(context.Background(), childSessionID); err == nil {
		usage.PromptTokens = persisted.PromptTokens
		usage.CompletionTokens = persisted.CompletionTokens
		usage.Cost = persisted.Cost
	}
	if messages, err := c.messages.List(context.Background(), childSessionID); err == nil {
		for _, msg := range messages {
			usage.ToolUseCount += len(msg.ToolCalls())
		}
	}
	return usage
}

func agentUsageSince(usage, baseline managedtask.AgentUsage) managedtask.AgentUsage {
	return managedtask.AgentUsage{
		PromptTokens:     max(usage.PromptTokens-baseline.PromptTokens, 0),
		CompletionTokens: max(usage.CompletionTokens-baseline.CompletionTokens, 0),
		Cost:             max(usage.Cost-baseline.Cost, 0),
		ToolUseCount:     max(usage.ToolUseCount-baseline.ToolUseCount, 0),
	}
}

func (c *coordinator) approveBackgroundSubagent(ctx context.Context, selected presetSubagent, sessionID, toolCallID string) (bool, error) {
	if !selected.requiresApproval {
		return false, nil
	}
	if c.permissions == nil {
		return false, errors.New("permission service is unavailable")
	}
	granted, err := c.permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID:   sessionID,
		ToolCallID:  toolCallID,
		ToolName:    AgentToolName,
		Action:      "delegate",
		Description: fmt.Sprintf("Allow %s to use its declared tools for this background run", selected.title),
		Params: map[string]any{
			"agent": selected.title,
			"tools": selected.tools,
		},
		Path: c.cfg.WorkingDir(),
	})
	if err != nil {
		return false, err
	}
	if !granted {
		return false, fmt.Errorf("permission denied for %s", selected.title)
	}
	return true, nil
}

func subagentRequiresApproval(allowedTools []string) bool {
	for _, name := range allowedTools {
		switch name {
		case "codebase_search", "crux_info", "crux_logs", "git_inspect", "job_list", "job_output", "lsp_call_hierarchy", "lsp_definition", "lsp_diagnostics", "lsp_references", "lsp_symbols", "ls", "memory_list", "project_status", "search", "skill_list", "sourcegraph", "task_list", "task_output", "traffic_logs", "view":
		default:
			return true
		}
	}
	return false
}

func (c *coordinator) customWildcardTools() []string {
	coderCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
	if !ok {
		return nil
	}
	allowed := make([]string, 0, len(coderCfg.AllowedTools))
	for _, name := range coderCfg.AllowedTools {
		if _, forbidden := forbiddenCustomAgentTools[name]; forbidden {
			continue
		}
		if name == "codebase_search" && !c.cfg.Config().Tools.CodebaseSearch.IsEnabled() {
			continue
		}
		if strings.HasPrefix(name, "lsp_") && len(c.cfg.Config().LSP) == 0 && c.cfg.Config().Options.AutoLSP != nil && !*c.cfg.Config().Options.AutoLSP {
			continue
		}
		allowed = append(allowed, name)
	}
	slices.Sort(allowed)
	return allowed
}
